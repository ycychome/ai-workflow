package issuedesk

import (
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed web/*
var web embed.FS

type App struct {
	store Store
	page  *template.Template
}

// NewApp 创建 HTTP 应用。调用者先执行迁移；令牌只从 Authorization 读取。
func NewApp(pool *pgxpool.Pool) http.Handler {
	return &App{Store{pool}, template.Must(template.ParseFS(web, "web/index.html"))}
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.Path == "/healthz" {
		if r.Method != "GET" {
			a.error(w, fail(405, "method_not_allowed", "不支持此方法"))
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/readyz" {
		if r.Method != "GET" {
			a.error(w, fail(405, "method_not_allowed", "不支持此方法"))
			return
		}
		if err := a.store.Pool.Ping(r.Context()); err != nil {
			a.error(w, fail(503, "not_ready", "数据库尚未就绪"))
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ready"})
		return
	}
	if r.URL.Path == "/" && r.Method == "GET" {
		title := r.URL.Query().Get("title")
		if title == "" {
			title = "工单工作台"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = a.page.Execute(w, map[string]any{"Title": title})
		return
	}
	if r.URL.Path == "/static/app.js" || r.URL.Path == "/static/app.css" {
		if r.Method != "GET" {
			a.error(w, fail(405, "method_not_allowed", "不支持此方法"))
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		b, _ := web.ReadFile("web/" + name)
		if strings.HasSuffix(name, ".js") {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		_, _ = w.Write(b)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		a.error(w, fail(404, "not_found", "接口不存在"))
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || len(auth) < 8 {
		a.error(w, fail(401, "unauthorized", "需要 Bearer 身份凭证"))
		return
	}
	actor, err := a.store.actor(r.Context(), strings.TrimPrefix(auth, "Bearer "))
	if err != nil {
		a.error(w, err)
		return
	}
	if r.URL.Path == "/api/me" && r.Method == "GET" {
		writeJSON(w, 200, actor)
		return
	}
	if r.URL.Path == "/api/tickets" {
		switch r.Method {
		case "GET":
			limit := 20
			if raw := r.URL.Query().Get("limit"); raw != "" {
				n, e := strconv.Atoi(raw)
				if e != nil {
					a.error(w, fail(400, "invalid_limit", "limit 必须是整数"))
					return
				}
				limit = n
			}
			page, e := a.store.list(r.Context(), actor, listInput{r.URL.Query().Get("status"), r.URL.Query().Get("q"), r.URL.Query().Get("assignee_id"), r.URL.Query().Get("cursor"), limit})
			if e != nil {
				a.error(w, e)
				return
			}
			writeJSON(w, 200, page)
		case "POST":
			var in createInput
			if e := decode(w, r, &in); e != nil {
				a.error(w, e)
				return
			}
			ticket, replay, e := a.store.create(r.Context(), actor, r.Header.Get("Idempotency-Key"), in)
			if e != nil {
				a.error(w, e)
				return
			}
			w.Header().Set("ETag", etag(ticket.Version))
			w.Header().Set("Location", "/api/tickets/"+strconv.FormatInt(ticket.ID, 10))
			if replay {
				w.Header().Set("Idempotency-Replayed", "true")
			}
			writeJSON(w, 201, ticket)
		default:
			a.error(w, fail(405, "method_not_allowed", "不支持此方法"))
		}
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/api/tickets/")
	parts := strings.Split(suffix, "/")
	if suffix == r.URL.Path || len(parts) > 2 {
		a.error(w, fail(404, "not_found", "接口不存在"))
		return
	}
	id, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || id < 1 {
		a.error(w, fail(400, "invalid_id", "工单编号无效"))
		return
	}
	if len(parts) == 2 {
		if parts[1] != "comments" {
			a.error(w, fail(404, "not_found", "接口不存在"))
			return
		}
		if r.Method == "GET" {
			items, e := a.store.comments(r.Context(), actor, id)
			if e != nil {
				a.error(w, e)
				return
			}
			writeJSON(w, 200, map[string]any{"items": items})
			return
		}
		if r.Method == "POST" {
			var in struct {
				Body string `json:"body"`
			}
			if e := decode(w, r, &in); e != nil {
				a.error(w, e)
				return
			}
			version, e := matchVersion(r)
			if e != nil {
				a.error(w, e)
				return
			}
			ticket, e := a.store.update(r.Context(), actor, id, version, updateInput{Comment: &in.Body})
			if e != nil {
				a.error(w, e)
				return
			}
			w.Header().Set("ETag", etag(ticket.Version))
			writeJSON(w, 200, ticket)
			return
		}
		a.error(w, fail(405, "method_not_allowed", "不支持此方法"))
		return
	}
	switch r.Method {
	case "GET":
		ticket, e := a.store.get(r.Context(), actor, id)
		if e != nil {
			a.error(w, e)
			return
		}
		w.Header().Set("ETag", etag(ticket.Version))
		writeJSON(w, 200, ticket)
	case "PATCH":
		var in updateInput
		if e := decode(w, r, &in); e != nil {
			a.error(w, e)
			return
		}
		version, e := matchVersion(r)
		if e != nil {
			a.error(w, e)
			return
		}
		ticket, e := a.store.update(r.Context(), actor, id, version, in)
		if e != nil {
			a.error(w, e)
			return
		}
		w.Header().Set("ETag", etag(ticket.Version))
		writeJSON(w, 200, ticket)
	case "DELETE":
		version, e := matchVersion(r)
		if e != nil {
			a.error(w, e)
			return
		}
		if e = a.store.remove(r.Context(), actor, id, version); e != nil {
			a.error(w, e)
			return
		}
		w.WriteHeader(204)
	default:
		a.error(w, fail(405, "method_not_allowed", "不支持此方法"))
	}
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if ct := strings.Split(r.Header.Get("Content-Type"), ";")[0]; ct != "application/json" {
		return fail(415, "unsupported_media_type", "请求正文必须为 application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fail(400, "invalid_json", "JSON 格式、字段或正文大小无效")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fail(400, "invalid_json", "正文只能包含一个 JSON 对象")
	}
	return nil
}
func matchVersion(r *http.Request) (int64, error) {
	raw := r.Header.Get("If-Match")
	if raw == "" {
		return 0, fail(428, "precondition_required", "更新或删除需要 If-Match 版本")
	}
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, fail(400, "invalid_version", "If-Match 必须为带双引号的版本号")
	}
	v, err := strconv.ParseInt(raw[1:len(raw)-1], 10, 64)
	if err != nil || v < 1 {
		return 0, fail(400, "invalid_version", "版本号无效")
	}
	return v, nil
}
func (a *App) error(w http.ResponseWriter, err error) {
	var p *problem
	if !errors.As(err, &p) {
		log.Printf("工单请求失败：%T", err)
		p = &problem{500, "internal_error", "服务暂时无法完成请求"}
	}
	writeJSON(w, p.status, map[string]APIError{"error": {p.code, p.message}})
}

// HTTPServer 配置有界超时，支持调用者优雅关闭。
func HTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
}
