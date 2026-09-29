package handler

import (
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"solarpanel/internal/config"
	"solarpanel/internal/db"
	"solarpanel/internal/model"
)

/*
图标内联（v3.0）

目标：前端拿到 public 接口数据的那一刻，卡片图标就已经在手，无需再发任何图片请求，
     从而彻底消除「冷启动 / 切换分组」时图标区域空白或延迟出现的问题。

做法：服务端把卡片图标读出来，转成 base64 data URI，随 public 接口一起下发。
     - 本地图标（/frontend/uploads/...）：直接读取内联
     - 远程图标（http...）：先查服务端缓存目录，命中即内联；
       未命中则后台异步拉取落盘，本次请求不内联（下一次刷新起即时显示）
     - favicon 型卡片：复用后端已有的 favicon 本地缓存

拉取健壮性（v3.0 修订）：
     - 携带浏览器 UA 与来源 Referer，避免被第三方站点拒绝
     - 以「实际内容」判定类型（魔数），不信任响应头，防止把 HTML 错误页当图片缓存
     - 落盘前校验确为图片，校验失败不落盘并允许重试
     - 启动后后台预热，把历史图标提前缓存好
*/

const (
	iconInlineMaxBytes = 256 * 1024
	// iconInlineBudget 单次 public 响应允许内联的 data URI 总字节上限，
	// 超出部分回退为同源 URL，避免响应体无限膨胀拖慢首屏。
	iconInlineBudget   = 512 * 1024
	iconCacheDirName   = "iconcache"
	iconFetchTimeout   = 10 * time.Second
	iconFetchMaxBytes  = 4 * 1024 * 1024
	iconFetchUA        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
)

// iconFetchGuard 保证同一 URL 在进程内不会被重复触发异步拉取
var iconFetchGuard sync.Map

var iconExtCandidates = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".svg"}

func iconCacheDir(cfg *config.Config) string {
	return filepath.Join(cfg.UploadDir, iconCacheDirName)
}

/* ------------------------------------------------------------
   data URI 内存缓存（v2.1.16 性能修复）
   早期版本每次 public 请求都要对每个卡片「读磁盘 + base64 编码」，
   卡片多时服务端耗时可达数百毫秒，页面迟迟拿不到数据，
   期间露出 WebView 底色 —— 表现为 App 先黑屏再出内容。
   这里按「路径 + 修改时间 + 大小」缓存编码结果，命中后零 I/O、零编码。
   ------------------------------------------------------------ */
type iconURIEntry struct {
	uri     string
	size    int64
	modTime time.Time
}

var iconURICache sync.Map // 绝对/缓存路径 -> *iconURIEntry

// cachedDataURI 带缓存地生成 data URI；文件变化时自动失效
func cachedDataURI(p string) string {
	if p == "" {
		return ""
	}
	st, err := os.Stat(p)
	if err != nil || st.Size() == 0 || st.Size() > iconInlineMaxBytes {
		return ""
	}
	if v, ok := iconURICache.Load(p); ok {
		e, _ := v.(*iconURIEntry)
		if e != nil && e.size == st.Size() && e.modTime.Equal(st.ModTime()) {
			return e.uri
		}
	}
	uri := cachedDataURI(p)
	if uri != "" {
		iconURICache.Store(p, &iconURIEntry{
			uri:     uri,
			size:    st.Size(),
			modTime: st.ModTime(),
		})
	}
	return uri
}

// warmInlineCache 预计算本地图标的 data URI，让首次 public 请求就命中缓存
func warmInlineCache(cfg *config.Config) {
	if cfg == nil {
		return
	}
	var items []model.Item
	if err := db.DB.Find(&items).Error; err != nil {
		return
	}
	for _, it := range items {
		if p := resolveIconAbsPath(cfg, it.IconValue); p != "" {
			cachedDataURI(p)
		}
		// favicon 型：预热后端已有的 favicon 本地缓存
		if it.IconType == "favicon" && it.URL != "" {
			if host := hostOfURL(it.URL); host != "" {
				if cached := checkFaviconCache(cfg, host); cached != "" {
					if p := resolveIconAbsPath(cfg, cached); p != "" {
						cachedDataURI(p)
					}
				}
			}
		}
	}
}

// 说明：图片类型判定复用 items_handler.go 中已有的 detectImageExt
// （magic byte 实测，覆盖 PNG/JPEG/GIF/ICO/WebP/SVG），此处不再重复实现。

// resolveIconAbsPath 把前端使用的图标路径映射为服务器磁盘绝对路径
func resolveIconAbsPath(cfg *config.Config, iconValue string) string {
	v := strings.TrimSpace(iconValue)
	if v == "" || cfg == nil {
		return ""
	}
	rel := ""
	switch {
	case strings.HasPrefix(v, "/frontend/uploads/"):
		rel = strings.TrimPrefix(v, "/frontend/uploads/")
	case strings.HasPrefix(v, "/uploads/"):
		rel = strings.TrimPrefix(v, "/uploads/")
	default:
		return ""
	}
	rel = filepath.FromSlash(rel)
	// 防目录穿越
	if strings.Contains(rel, "..") {
		return ""
	}
	return filepath.Join(cfg.UploadDir, rel)
}

// iconCachePathFor 返回远程图标在服务端缓存中的落盘路径（不存在则返回空）
func iconCachePathFor(cfg *config.Config, rawURL string) string {
	dir := iconCacheDir(cfg)
	h := md5Hex(rawURL)
	for _, ext := range iconExtCandidates {
		p := filepath.Join(dir, h+ext)
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			return p
		}
	}
	return ""
}

// readImageFile 读取图片文件；返回 data URI，失败/非图片返回空
func readImageFile(p string) string {
	st, err := os.Stat(p)
	if err != nil || st.Size() == 0 || st.Size() > iconInlineMaxBytes {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, st.Size())
	if _, err := io.ReadFull(f, buf); err != nil {
		return ""
	}
	// 以实际内容判定类型；非图片直接判定为坏文件（例如缓存了 HTML 错误页）
	ct := detectImageExt(buf)
	if ct == "" {
		return ""
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(buf)
}

// hostOfURL 取 URL 的 host，失败返回空
func hostOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// ensureRemoteIconAsync 后台拉取远程图标并落盘，供后续请求内联
func ensureRemoteIconAsync(cfg *config.Config, rawURL string) {
	if cfg == nil || rawURL == "" {
		return
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return
	}
	if isPrivateHost(hostOfURL(rawURL)) {
		return // 内网地址不代理，避免 SSRF
	}
	if _, loaded := iconFetchGuard.LoadOrStore(rawURL, true); loaded {
		return
	}
	go func() {
		if !fetchAndCacheIcon(cfg, rawURL) {
			// 失败则释放，允许后续请求重试
			iconFetchGuard.Delete(rawURL)
		}
	}()
}

func fetchAndCacheIcon(cfg *config.Config, rawURL string) bool {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	// 很多站点/CDN 会拒绝默认的 Go UA，带上浏览器标识与来源
	req.Header.Set("User-Agent", iconFetchUA)
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/svg+xml,image/*,*/*;q=0.8")
	if u, err := url.Parse(rawURL); err == nil {
		req.Header.Set("Referer", u.Scheme+"://"+u.Host+"/")
	}

	client := &http.Client{Timeout: iconFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, iconFetchMaxBytes+1))
	if err != nil || len(body) == 0 || len(body) > iconFetchMaxBytes {
		return false
	}

	// 以实际内容判定类型；不是图片就不落盘（防止把 HTML 错误页缓存成 .png）
	ext := detectImageExt(body)
	if ext == "" {
		return false
	}
	if ext == ".bmp" {
		return false // bmp 体积大，不适合内联
	}

	dir := iconCacheDir(cfg)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Println("[icon] 缓存目录不可写:", err)
		return false
	}

	// 先清掉同 URL 可能存在的旧扩展名文件，避免多个副本
	h := md5Hex(rawURL)
	for _, e := range iconExtCandidates {
		_ = os.Remove(filepath.Join(dir, h+e))
	}

	if err := os.WriteFile(filepath.Join(dir, h+ext), body, 0644); err != nil {
		return false
	}
	return true
}

// IconInlineData 生成卡片图标的 data URI；无法内联时返回空串
func IconInlineData(cfg *config.Config, iconType, iconValue, itemURL string) string {
	if cfg == nil {
		return ""
	}
	v := strings.TrimSpace(iconValue)
	if iconType == "text" || v == "" {
		return ""
	}

	// 1) 本地图标：直接内联
	if p := resolveIconAbsPath(cfg, v); p != "" {
		if d := cachedDataURI(p); d != "" {
			return d
		}
	}

	// 2) 远程图标：命中服务端缓存则内联，否则后台拉取（下次生效）
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		if p := iconCachePathFor(cfg, v); p != "" {
			if d := cachedDataURI(p); d != "" {
				return d
			}
			// 缓存内容损坏（非图片）→ 删除并允许重新拉取
			_ = os.Remove(p)
			iconFetchGuard.Delete(v)
		}
		ensureRemoteIconAsync(cfg, v)
		return ""
	}

	// 3) favicon 型：复用后端 favicon 本地缓存
	if iconType == "favicon" && itemURL != "" {
		if host := hostOfURL(itemURL); host != "" {
			if cached := checkFaviconCache(cfg, host); cached != "" {
				if p := resolveIconAbsPath(cfg, cached); p != "" {
					if d := cachedDataURI(p); d != "" {
						return d
					}
				}
			}
		}
	}
	return ""
}

// WarmupIcons 启动后在后台把所有远程图标缓存到本地，供后续请求内联。
// 仅对远程直链生效；本地图标无需预热。
func WarmupIcons(cfg *config.Config) {
	if cfg == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Println("[icon] 预热异常:", r)
			}
		}()
		// 先预计算本地图标的 data URI，让首次 public 请求就命中内存缓存（零读盘、零编码）
		warmInlineCache(cfg)

		var items []model.Item
		if err := db.DB.Where("icon_type = ?", "image").Find(&items).Error; err != nil {
			return
		}
		n := 0
		for _, it := range items {
			v := strings.TrimSpace(it.IconValue)
			if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
				continue
			}
			if iconCachePathFor(cfg, v) != "" {
				continue
			}
			ensureRemoteIconAsync(cfg, v)
			n++
			if n%3 == 0 {
				time.Sleep(300 * time.Millisecond) // 平滑节奏，避免并发过高
			}
		}
		if n > 0 {
			log.Printf("[icon] 已发起 %d 个远程图标的后台缓存", n)
		}
	}()
}
