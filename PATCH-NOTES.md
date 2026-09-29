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

## 四、本次改动：应用图标延迟加载优化（仅前端）

目标：切换分组时卡片图标不再延迟出现。未触碰分组图标（v2.1.16 新功能）任何代码。

改动文件：`frontend/assets/js/index.js`（两版同步）

### 1. 新增图标缓存与预取模块（`buildCard` 之前）

- `__iconUrlCache`：`host -> 已验证可用的图标 URL`
- `__iconDeadHost`：记录所有候选源均失败的 host，避免重复发请求
- `__prefetchIcon(url)`：串行探测候选源，成功后写入缓存
- `__prefetchAllIcons()`：首屏后后台静默预取所有分组内 favicon 型卡片图标
- `__groupSectionCache`：`groupId -> section`，导航栏模式下复用已渲染 DOM

### 2. `buildCard()` 图标分支重写

原实现：每次新建 `<img>` 并从第一个源开始串行回退，
`onload` 之前图标区是**空白**（文字图标只在全部失败后兜底）。

新实现：

- 命中 `__iconUrlCache` → 直接复用，浏览器缓存瞬时命中
- host 在 `__iconDeadHost` → 直接用文字图标，不发请求
- 首次 → **先画文字图标占位（立即可见）**，后台探测成功后原子替换

图片一律先在内存中 `new Image()` 加载完成再替换占位，全程无空白帧。

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
7. 检查官方包是否给 `ItemGroup` 增加了新字段，若有则同步到
   `SolarPanel-Docker/internal/model/models.go`
8. 提交并打 `v*.*.*` 标签触发镜像构建

建议每次升级前先 `git tag` 打一个基线快照，便于回滚对比。
