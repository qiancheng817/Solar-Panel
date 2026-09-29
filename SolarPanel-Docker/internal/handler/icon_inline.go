package handler

import (
	"encoding/base64"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"solarpanel/internal/config"
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

限制：单个图标超过 iconInlineMaxBytes 不内联，避免 public 响应体过大。
*/

const (
	iconInlineMaxBytes = 96 * 1024
	iconCacheDirName   = "iconcache"
	iconFetchTimeout   = 8 * time.Second
	iconFetchMaxBytes  = 2 * 1024 * 1024
)

// iconFetchGuard 保证同一 URL 在进程内不会被重复触发异步拉取
var iconFetchGuard sync.Map

var iconExtCandidates = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".svg"}

var iconCTExt = map[string]string{
	"image/png":                ".png",
	"image/jpeg":               ".jpg",
	"image/jpg":                ".jpg",
	"image/gif":                ".gif",
	"image/webp":               ".webp",
	"image/x-icon":             ".ico",
	"image/vnd.microsoft.icon": ".ico",
	"image/svg+xml":            ".svg",
}

func iconCacheDir(cfg *config.Config) string {
	return filepath.Join(cfg.UploadDir, iconCacheDirName)
}

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

// dataURIFromPath 读取图片文件并转成 data URI；超限/失败返回空
func dataURIFromPath(p string) string {
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
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))
	if ct == "" {
		ct = http.DetectContentType(buf)
	}
	if ct == "" || !strings.HasPrefix(ct, "image/") {
		ct = "image/png"
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
		ok := fetchAndCacheIcon(cfg, rawURL)
		if !ok {
			// 失败则释放，允许后续请求重试
			iconFetchGuard.Delete(rawURL)
		}
	}()
}

func fetchAndCacheIcon(cfg *config.Config, rawURL string) bool {
	client := &http.Client{Timeout: iconFetchTimeout}
	resp, err := client.Get(rawURL)
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
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	ext := iconCTExt[ct]
	if ext == "" {
		if e := strings.ToLower(filepath.Ext(hostOfURL(rawURL))); e != "" {
			ext = e
		}
	}
	if ext == "" {
		ext = iconCTExt[http.DetectContentType(body)]
	}
	if ext == "" {
		ext = ".png"
	}
	valid := false
	for _, e := range iconExtCandidates {
		if e == ext {
			valid = true
			break
		}
	}
	if !valid {
		ext = ".png"
	}

	dir := iconCacheDir(cfg)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Println("[icon] 缓存目录不可写:", err)
		return false
	}
	p := filepath.Join(dir, md5Hex(rawURL)+ext)
	if err := os.WriteFile(p, body, 0644); err != nil {
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
		if d := dataURIFromPath(p); d != "" {
			return d
		}
	}

	// 2) 远程图标：命中服务端缓存则内联，否则后台拉取（下次生效）
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		if p := iconCachePathFor(cfg, v); p != "" {
			if d := dataURIFromPath(p); d != "" {
				return d
			}
		}
		ensureRemoteIconAsync(cfg, v)
		return ""
	}

	// 3) favicon 型：复用后端 favicon 本地缓存
	if iconType == "favicon" && itemURL != "" {
		if host := hostOfURL(itemURL); host != "" {
			if cached := checkFaviconCache(cfg, host); cached != "" {
				if p := resolveIconAbsPath(cfg, cached); p != "" {
					if d := dataURIFromPath(p); d != "" {
						return d
					}
				}
			}
		}
	}
	return ""
}
