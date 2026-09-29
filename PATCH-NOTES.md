# SolarPanel 2.1.16 基线说明与改造记录

本仓库已包含 **官方 v2.1.16 的完整有效代码**，后续优化/升级可直接基于本仓库进行，
无需再回上游仓库或官方升级源取代码。

---

## 一、为什么仓库里原本没有 2.1.16

上游 `Ozero-top/Solar-Panel`（及本 fork）的 git 仓库源码停留在 **v2.1.13**
（`frontend/assets/js/api.js` 中 `APP_VERSION = 'v2.1.13'`），
但 README 已写到 v2.1.17。作者是通过**在线升级包**分发新版，仓库未同步。

v2.1.16 的真实代码从官方升级源取得：

```
升级源索引: https://updates.ozero.top/versions.json
全量包:     https://updates.ozero.top/web/SolarPanel-full-v2.1.16.zip
MD5:        7f006e03181388d70cf39ce48ad7f363
大小:       6210279 字节
包内标识:   upgrade-manifest.json -> "ver":"v2.1.16","full":true
```

> 若将来需要取更新的版本（如 v2.1.17、v2.1.18），只需把上面 URL 里的版本号替换即可，
> 索引文件里每个版本都有 `web.full.url` 与 `md5`。

---

## 二、仓库内容构成

| 目录 | 内容 | 来源 |
|---|---|---|
| `SolarPanel-web/backend/api`、`lib` | PHP 后端 v2.1.16 | 官方全量包 |
| `SolarPanel-web/frontend` | 前端 v2.1.16 | 官方全量包（含图标优化补丁） |
| `SolarPanel-web/sql`、`index.html`、`sw.js`、`manifest.json` | v2.1.16 | 官方全量包 |
| `SolarPanel-web/backend/config.php` | 运行态数据库配置 | **保留未覆盖**（官方包不含） |
| `SolarPanel-Docker/internal`、`main.go` | Go 后端 | 本仓库原有（v2.1.13）+ 分组图标字段补全 |
| `SolarPanel-Docker/frontend` | 前端 v2.1.16 | 官方全量包 + API 路径修正 + 图标优化补丁 |

---

## 三、两版前端的关键差异：API 基址（务必注意）

官方全量包是 **web 版**，其前端 API 基址为：

```js
const API_BASE = '../backend/api/';
```

而 Docker 版 Go 后端提供的路由是 `/api/*.php`，基址必须是：

```js
const API_BASE = '/api/';
```

官方 v2.1.16 的 web 包里，`index.js` 另有 **6 处硬编码** 的 `'../backend/api/...'`
（weather / items.sort_batch / news.all / news.source / auth.guest_login / wallpaper.daily），
不走 `API_BASE`。**直接把 web 包前端塞进 Docker 版，这些接口会全部 404** ——
这正是 v2.1.16 changelog 中「修复 Docker 404 错误」所针对的问题。

本仓库的处理：Docker 版前端已把全部 `'../backend/api/` 统一改为 `'/api/`，
共涉及 `index.js`、`admin.js`、`login.js` 三个文件。

**升级新版本时，这一步必须重做。**

---

## 三之二、v3.0：服务端内联图标（彻底消除等待）

前两版补丁都是「前端优化加载时机」，仍依赖网络请求，冷启动时首个分组
依然要等图片下载。**v3.0 改为服务端内联，从根本上消除等待**：

- 后端 `public` 接口在返回卡片数据时，把图标读出并转成 **base64 data URI**
  （字段 `icon_data`），随 JSON 一起下发
- 前端 `buildCard` 发现 `icon_data` 就直接渲染 —— **零图片请求**，
  冷启动与切换分组都不再有等待
- 同时移除文字/底色占位（按需求），图标未就绪时不再有任何中间态

### 后端改动（Docker / Go）

| 文件 | 说明 |
|---|---|
| `internal/handler/icon_inline.go` | 新增。图标内联与远程图标服务端缓存 |
| `internal/model/models.go` | `Item` 增加 `IconData`（`gorm:"-"`，非持久化） |
| `internal/handler/public_handler.go` | `PublicHandler` 持有 `cfg`，返回时填充 `icon_data` |
| `main.go` | `NewPublicHandler(cfg)`；uploads 静态资源加 `Cache-Control` |

内联规则：

- **本地图标**（`/frontend/uploads/...`）→ 直接读盘内联
- **远程图标**（http…）→ 查服务端缓存 `uploads/iconcache/<md5(url)>.<ext>`，
  命中则内联；未命中则**后台异步拉取落盘**，本次不内联（下次刷新起即时）
- **favicon 型** → 复用后端已有的 favicon 本地缓存
- 单图标超过 96 KB 不内联，避免 public 响应体过大
- 远程拉取有 SSRF 防护（`isPrivateHost`）与 8 秒超时、2 MB 大小限制

#### 拉取健壮性修订（首次实测后）

第一版内联上线后，仍有少数图标未能内联（表现为短暂延迟后才出现）。
定位到两个原因并修复：

1. **拉取不带浏览器标识**：Go 默认 UA 为 `Go-http-client/1.1`，
   部分站点 / CDN 会直接拒绝或返回 HTML 错误页。
   → 改为携带 Chrome UA + `Accept: image/*` + 来源站 `Referer`
2. **未校验响应内容是否真的是图片**：旧逻辑只按响应头推断扩展名，
   会把 HTML 错误页当成 `.png` 落盘，之后生成坏 data URI 并**永久占用缓存**
   （删不掉、也永远不会重试）。
   → 改为以**实际内容**判定类型（`http.DetectContentType` 魔数 + SVG 前缀判断），
   非图片一律不落盘；内联时若发现缓存文件内容异常，**自动删除并释放重试锁**

配套改进：

- 内联上限 96 KB → **256 KB**
- 落盘前清理同 URL 的旧扩展名文件，避免多份副本
- 新增 **`WarmupIcons()`**：容器启动后在后台遍历所有远程图标并逐个缓存，
  重启后等待数秒，之后每次访问都是内联即时显示
- 前端新增 **`__scheduleIconBackfill()`**：首屏渲染 3.5 秒后，
  若仍有卡片图标未显示，静默重取一次数据并**只替换图标位**（不重建 DOM），
  兜住任何原因导致的未内联

#### 性能修复：内联导致首屏变慢（App 黑屏）

**现象**：内联上线后，App 中从别的应用切回时先黑屏再出内容；
而内联之前虽然图标有延迟，但从不黑屏。

**根因**：内联把图标从「页面渲染之后再加载」挪到了「页面渲染之前必须拿到」，
等于把「图标慢」换成了「整页慢」——页面结构被一起推迟，期间露出
WebView 底色（App 的 `app_background`，系统深色下为 `#12161A`）形成黑屏。

具体开销有三处：

1. **每次 public 请求都重新读盘 + base64 编码**（无缓存），30+ 卡片即 30+ 次 I/O
2. **静态资源完全没有缓存头**：CSS/JS 经 `c.Data()` 直出，无 ETag/Last-Modified，
   每次重载都全量下载（仅 CSS 就约 150 KB）
3. **public 接口原本是 `no-store`**，切回时一律重新拉取完整响应

**修复**：

| 位置 | 改动 |
|---|---|
| `icon_inline.go` | 新增 `iconURICache`：按「路径 + 修改时间 + 大小」缓存 data URI，命中后零 I/O 零编码 |
| `icon_inline.go` | `warmInlineCache()`：启动时预计算全部本地图标，首次请求即命中缓存 |
| `public_handler.go` | 新增 `iconInlineBudget`（512 KB）总量上限，超出回退同源 URL |
| `public_handler.go` | `Cache-Control` 改为 `private, max-age=60, stale-while-revalidate=600` |
| `main.go` | 带 `?v=` 版本参数的静态资源长缓存（immutable）；HTML 保持 `no-cache` |

`stale-while-revalidate` 是关键：缓存过期后仍**先用旧内容立即渲染**，
再后台静默更新，因此从别的应用切回时始终「直接显示已有内容」，不会空白。

> 前端 `fetch` 未设置 `cache: 'no-store'`（默认 `default`），遵循 HTTP 缓存头，
> 上述后端缓存策略可正常生效。

### web 版（PHP）说明

内联目前**只在 Docker / Go 版实现**。PHP 版 `public.php` 未改动，
其 `icon_data` 恒为空，自动回退到前端预加载逻辑，功能不受影响。

---

## 四、本次改动：应用图标延迟加载优化（仅前端）

目标：切换分组时卡片图标不再延迟出现。未触碰分组图标（v2.1.16 新功能）任何代码。

改动文件：`frontend/assets/js/index.js`（两版同步）

> **注意**：本补丁覆盖 `image` / `favicon` / `text` **全部三种**图标类型。
> 早期版本只改了 `favicon` 分支，导致图标类型为 `image` 的卡片仍然空白，
> 已于第二次修订中全类型覆盖。

### 1. 新增图标缓存与预取模块（`buildCard` 之前）

- `__imgCache`：**图片 URL -> 已加载完成的 HTMLImageElement**
  （同一对象直接挂进 DOM，浏览器不再重新请求；这是消除延迟的关键）
- `__imgFailed`：图片 URL -> 加载失败，避免重复请求
- `__preloadImage(src)`：统一的图片预加载入口，成功时写入 `__imgCache`
- `__iconUrlCache`：`host -> 已验证可用的 favicon URL`
- `__iconDeadHost`：记录所有候选源均失败的 host
- `__prefetchIcon(url)`：串行探测候选源（内部复用 `__preloadImage`）
- `__prefetchAllIcons()`：首屏后经 `requestIdleCallback` 空闲预取
  **全部三种类型**（image 直链 + favicon 探测），避免与首屏争带宽
- `__groupSectionCache`：`groupId -> section`，导航栏模式下复用已渲染 DOM

### 2. `buildCard()` 图标分支重写

原实现：每次新建 `<img>` 立即 `appendChild`，
`onload` 之前图标位只显示 `.card .icon` 的默认底色（一片空色块）。

新实现（`paintImage` + `mountImage` 统一处理 image 与 favicon）：

1. 命中 `__imgCache` → **直接挂载已加载好的 Image 对象，零等待**
2. URL 在 `__imgFailed` / host 在 `__iconDeadHost` → 直接用文字图标，不发请求
3. 首次 → **先画文字图标占位（立即可见）**，加载完成后原子替换

`mountImage()` 会清理文字占位残留：移除 `icon-grid-cn` / `icon-wrap-en`
布局类，并清空内联 `background`（内联优先级高于 `.has-img`，不清会导致
图片显示在彩色底色上）。

### 3. `renderNavbarCardsOnly()` 复用分组 DOM

原实现每次切换都 `wrap.innerHTML = ''` 后重建全部卡片，图标重新加载。
新实现优先复用 `__groupSectionCache` 中已渲染的 section，切换不再重建。

### 4. 缓存失效

`renderGroups()` 是数据变化（首次加载 / 排序 / 编辑 / 权限变更）的唯一入口，
在其中清空 `__groupSectionCache` 并触发 `__prefetchAllIcons()`。
切换分组只走 `renderNavbarCardsOnly()`，不触发失效。

---

## 五、Go 后端补全：分组图标字段

Docker 版 Go 后端源码仍是 v2.1.13，`ItemGroup` 没有 `icon` 字段，
若不补全，v2.1.16 的「分组图标可视化选择」在 Docker 版会失效。

改动：

- `internal/model/models.go` — `ItemGroup` 增加
  `Icon string \`gorm:"size:8;not null;default:''" json:"icon"\``
- `internal/handler/groups_handler.go` — `Edit` 接收并保存 `icon`
  （去 `<>`、按 rune 截断到 8，与 PHP 版行为一致）

GORM `AutoMigrate` 会自动为已有 SQLite 库补上该列，无需手动迁移。

---

## 六、构建镜像

已配置 `.github/workflows/docker-build.yml`：

- 默认构建 `linux/amd64`（飞牛 x86 直接用）
- 推送到 `ghcr.io/qiancheng817/solar-panel`，标签 `2.1.16` 与 `latest`
- 触发方式：push `v*` 标签，或在 Actions 页面手动 `Run workflow`
  （可自定义标签与平台，如 `linux/arm64`）

手动触发步骤：仓库 → Actions → **Build SolarPanel Image** → Run workflow。

拉取：

```bash
docker pull ghcr.io/qiancheng817/solar-panel:2.1.16
```

> GHCR 的包默认私有，需在仓库 Packages 设置里改为 Public 才能免登录拉取。

---

## 七、后续升级流程（重要）

1. 从 `https://updates.ozero.top/versions.json` 取目标版本的 `web.full.url` 与 `md5`
2. 下载全量包并校验 MD5
3. 用包内 `frontend/` 覆盖 `SolarPanel-web/frontend`
   （**保留 `backend/config.php`**）
4. 用包内 `backend/api`、`backend/lib` 覆盖 `SolarPanel-web/backend` 对应目录
5. **重新生成 Docker 版前端**：把包内 `frontend/` 覆盖到
   `SolarPanel-Docker/frontend/`，然后把其中所有
   `'../backend/api/` 替换为 `'/api/'`
6. **重新应用图标优化补丁**：参见本文档第四节，
   比对 `SolarPanel-web/frontend/assets/js/index.js` 与官方包的差异并还原
7. **提升 Service Worker 缓存版本号**：把两版 `frontend/sw.js` 中的
   `CACHE_NAME` 递增（如 `sp-cache-v19` → `sp-cache-v20`）。
   **这一步不能省**：`sw.js` 对 JS/CSS 采用 stale-while-revalidate
   （`return cached || fetchPromise`），会**优先返回旧缓存**，
   不提升版本号的话镜像更新后首次打开仍是旧代码，刷新一次才生效，
   极易被误判为「改动无效」。
8. 检查官方包是否给 `ItemGroup` 增加了新字段，若有则同步到
   `SolarPanel-Docker/internal/model/models.go`
9. 提交并打 `v*.*.*` 标签触发镜像构建

建议每次升级前先 `git tag` 打一个基线快照，便于回滚对比。

### 用户侧更新镜像后若仍显示旧界面

按顺序排查：

1. `docker compose pull && docker compose up -d`（确认拉到了新镜像）
2. 浏览器**硬刷新**（Ctrl+Shift+R / Cmd+Shift+R）
3. 仍不行则手动清站点数据：DevTools → Application → Service Workers →
   Unregister，再 Clear storage
4. 确认镜像构建时间：仓库 → Packages → solar-panel → 版本列表
