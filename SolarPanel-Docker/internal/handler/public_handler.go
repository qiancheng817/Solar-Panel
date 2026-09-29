package handler

import (
	"net/http"

	"solarpanel/internal/auth"
	"solarpanel/internal/config"
	"solarpanel/internal/db"
	"solarpanel/internal/model"

	"github.com/gin-gonic/gin"
)

type PublicHandler struct {
	cfg *config.Config
}

func NewPublicHandler(cfg *config.Config) *PublicHandler {
	return &PublicHandler{cfg: cfg}
}

type GroupWithItems struct {
	model.ItemGroup
	Items []model.Item `json:"items"`
}

func (h *PublicHandler) Public(c *gin.Context) {
	// v2.1.16：允许浏览器/WebView 短暂复用（private = 不进任何共享代理）。
	// 从别的应用切回时若仍在有效期内，取缓存零往返，页面直接呈现已有内容，
	// 避免重新拉取（含内联图标的大响应）期间露出 WebView 底色形成黑屏。
	// max-age 内直接取缓存；超出后先用陈旧缓存立即渲染，再后台静默更新，
	// 因此从别的应用切回时始终「直接显示已有内容」，不会因等待响应而空白。
	c.Header("Cache-Control", "private, max-age=60, stale-while-revalidate=600")
	var settingsRows []model.Setting
	if err := db.DB.Find(&settingsRows).Error; err != nil {
		Fail(c, http.StatusInternalServerError, "读取设置失败")
		return
	}
	settings := make(map[string]string, len(settingsRows))
	for _, s := range settingsRows {
		settings[s.ConfigName] = s.ConfigValue
	}

	isAdmin := false
	var userInfo gin.H = nil
	guestRequired := false
	val, ok := c.Get("session")
	if ok {
		if sess, ok := val.(*auth.Session); ok {
			if sess.Role == "admin" || sess.Role == "editor" || sess.Role == "viewer" {
				isAdmin = true // 非管理员也可以看隐藏分组（viewer 只读但可见全部）
			}
			// 访客（guest 角色）也认为已登录过访客密码
			if sess.Role != "guest" {
				var u model.User
				if err := db.DB.First(&u, sess.UID).Error; err == nil {
					userInfo = gin.H{
						"username": u.Username,
						"name":     u.Name,
						"role":     u.Role,
					}
				}
			} else {
				userInfo = gin.H{"role": "guest"}
			}
		}
	} else {
		// 未登录 — 检查访客密码是否开启
		if settings["guest_access_enabled"] == "1" {
			guestRequired = true
		}
	}

	// 访客密码未验证：不返回任何分组/卡片数据，彻底隐藏前端信息
	if guestRequired {
		Ok(c, gin.H{
			"settings":       settings,
			"groups":         []GroupWithItems{},
			"user":           nil,
			"guest_required": true,
		})
		return
	}

	q := db.DB.Order("sort asc, id asc")
	if !isAdmin {
		q = q.Where("is_visible = ?", 1)
	}
	var groups []model.ItemGroup
	if err := q.Find(&groups).Error; err != nil {
		Fail(c, http.StatusInternalServerError, "读取分组失败")
		return
	}

	var items []model.Item
	if err := db.DB.Order("sort asc, id asc").Find(&items).Error; err != nil {
		Fail(c, http.StatusInternalServerError, "读取卡片失败")
		return
	}

	grouped := make([]GroupWithItems, 0, len(groups))
	inlineUsed := 0
	for _, g := range groups {
		gw := GroupWithItems{ItemGroup: g, Items: []model.Item{}}
		for _, it := range items {
			if it.GroupID != g.ID {
				continue
			}
			// 内联卡片图标，前端拿到数据即可直接渲染，无需再发图片请求。
			// 预算保护：累计超过上限后不再内联，超出的卡片回退为同源 URL
			// （uploads 已带长缓存头，浏览器缓存后同样很快）。
			if inlineUsed < iconInlineBudget {
				if d := IconInlineData(h.cfg, it.IconType, it.IconValue, it.URL); d != "" {
					inlineUsed += len(d)
					it.IconData = d
				}
			}
			gw.Items = append(gw.Items, it)
		}
		grouped = append(grouped, gw)
	}

	Ok(c, gin.H{
		"settings":       settings,
		"groups":         grouped,
		"user":           userInfo,
		"guest_required": guestRequired,
	})
}
