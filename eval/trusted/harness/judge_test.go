// 可信外部判定进程：不导入候选模块；候选 stdout 永不参与测试事件解析。
package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type Ticket struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Version int64  `json:"version"`
}
type ticketPage struct {
	Items      []Ticket `json:"items"`
	NextCursor string   `json:"next_cursor"`
}
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func jsonBytes(v any) []byte { b, _ := json.Marshal(v); return b }
func etag(v int64) string    { return fmt.Sprintf(`"%d"`, v) }

type evalRig struct {
	pool   *pgxpool.Pool
	server struct{ URL string }
	client *http.Client
	t      *testing.T
}

func newEvalRig(t *testing.T) *evalRig {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	socket := os.Getenv("AI_WORKFLOW_JUDGE_SOCKET")
	if dsn == "" || socket == "" {
		t.Fatal("外部判定缺少隔离服务配置")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	t.Cleanup(transport.CloseIdleConnections)
	r := &evalRig{client: client, t: t}
	r.server.URL = "http://candidate"
	deadline := time.Now().Add(20 * time.Second)
	for {
		response, err := client.Get(r.server.URL + "/healthz")
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("候选服务未能提供真实健康接口")
		}
		time.Sleep(100 * time.Millisecond)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal("可信数据库连接失败")
	}
	t.Cleanup(pool.Close)
	r.pool = pool
	if _, err = pool.Exec(context.Background(), `DELETE FROM comments; DELETE FROM tickets; DELETE FROM idempotency_keys`); err != nil {
		t.Fatal(err)
	}
	return r
}
func (r *evalRig) request(method, path, token string, body any, headers map[string]string) (int, []byte, http.Header) {
	r.t.Helper()
	var input io.Reader
	if body != nil {
		input = strings.NewReader(string(jsonBytes(body)))
	}
	req, err := http.NewRequest(method, r.server.URL+path, input)
	if err != nil {
		r.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	return resp.StatusCode, b, resp.Header
}
func (r *evalRig) create(title string) Ticket {
	r.t.Helper()
	status, b, _ := r.request("POST", "/api/tickets", "demo-alpha-requester", map[string]string{"title": title, "body": "合成正文"}, map[string]string{"Idempotency-Key": "key-" + title})
	if status != 201 {
		r.t.Fatalf("创建失败：%d %s", status, b)
	}
	var ticket Ticket
	if err := json.Unmarshal(b, &ticket); err != nil {
		r.t.Fatal(err)
	}
	return ticket
}
func (r *evalRig) ticket(id int64) Ticket {
	r.t.Helper()
	status, b, _ := r.request("GET", fmt.Sprintf("/api/tickets/%d", id), "demo-alpha-agent", nil, nil)
	if status != 200 {
		r.t.Fatalf("读取失败：%d %s", status, b)
	}
	var ticket Ticket
	if err := json.Unmarshal(b, &ticket); err != nil {
		r.t.Fatal(err)
	}
	return ticket
}
func TestHiddenT01Pagination(t *testing.T) {
	r := newEvalRig(t)
	for i := 0; i < 7; i++ {
		r.create(fmt.Sprintf("分页-%d", i))
	}
	if _, err := r.pool.Exec(context.Background(), `UPDATE tickets SET created_at='2025-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	cursor := ""
	last := int64(1 << 62)
	for page := 0; page < 5; page++ {
		status, b, _ := r.request("GET", "/api/tickets?limit=2&cursor="+url.QueryEscape(cursor), "demo-alpha-agent", nil, nil)
		if status != 200 {
			t.Fatalf("分页失败：%d %s", status, b)
		}
		var p ticketPage
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		for _, item := range p.Items {
			if seen[item.ID] || item.ID >= last {
				t.Fatalf("重复或顺序不稳定：%d", item.ID)
			}
			seen[item.ID] = true
			last = item.ID
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("分页遗漏：%d", len(seen))
	}
}
func TestHiddenT02StateMachine(t *testing.T) {
	r := newEvalRig(t)
	ticket := r.create("状态机")
	path := fmt.Sprintf("/api/tickets/%d", ticket.ID)
	for _, state := range []string{"in_progress", "resolved", "closed"} {
		status, b, _ := r.request("PATCH", path, "demo-alpha-agent", map[string]string{"status": state}, map[string]string{"If-Match": etag(ticket.Version)})
		if status != 200 {
			t.Fatalf("合法流转失败：%d %s", status, b)
		}
		_ = json.Unmarshal(b, &ticket)
	}
	status, _, _ := r.request("PATCH", path, "demo-alpha-admin", map[string]string{"status": "open"}, map[string]string{"If-Match": etag(ticket.Version)})
	if status != 409 {
		t.Fatalf("终态应拒绝重开，实际 %d", status)
	}
	if r.ticket(ticket.ID).Status != "closed" {
		t.Fatal("非法流转修改了持久状态")
	}
}
func TestHiddenT03Transaction(t *testing.T) {
	r := newEvalRig(t)
	ticket := r.create("事务原值")
	_, err := r.pool.Exec(context.Background(), `CREATE FUNCTION reject_eval_comment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic failure'; END $$; CREATE TRIGGER reject_eval_comment BEFORE INSERT ON comments FOR EACH ROW EXECUTE FUNCTION reject_eval_comment()`)
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ := r.request("PATCH", fmt.Sprintf("/api/tickets/%d", ticket.ID), "demo-alpha-agent", map[string]string{"title": "不应落库", "comment": "触发事务失败"}, map[string]string{"If-Match": etag(ticket.Version)})
	if status != 500 {
		t.Fatalf("故障注入应失败，实际 %d", status)
	}
	after := r.ticket(ticket.ID)
	if after.Title != ticket.Title || after.Version != ticket.Version {
		t.Fatal("评论失败却提交了工单更新")
	}
}
func TestHiddenT04Tenant(t *testing.T) {
	r := newEvalRig(t)
	ticket := r.create("租户甲私有工单")
	for _, suffix := range []string{"", "/comments"} {
		status, b, _ := r.request("GET", fmt.Sprintf("/api/tickets/%d%s", ticket.ID, suffix), "demo-beta-admin", nil, nil)
		if status != 404 || strings.Contains(string(b), ticket.Title) {
			t.Fatalf("跨租户数据泄露：%d", status)
		}
	}
	status, b, _ := r.request("GET", "/api/tickets", "demo-beta-admin", nil, nil)
	if status != 200 || strings.Contains(string(b), ticket.Title) {
		t.Fatal("列表跨租户泄露")
	}
	status, _, _ = r.request("PATCH", fmt.Sprintf("/api/tickets/%d", ticket.ID), "demo-alpha-requester", map[string]int64{"assignee_id": 102}, map[string]string{"If-Match": etag(ticket.Version)})
	if status != 403 {
		t.Fatalf("客户不应有分派权限：%d", status)
	}
}
func TestHiddenT05Concurrency(t *testing.T) {
	r := newEvalRig(t)
	ticket := r.create("并发")
	start := make(chan struct{})
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for _, title := range []string{"修改一", "修改二"} {
		wg.Add(1)
		go func(title string) {
			defer wg.Done()
			<-start
			req, _ := http.NewRequest("PATCH", fmt.Sprintf("%s/api/tickets/%d", r.server.URL, ticket.ID), strings.NewReader(string(jsonBytes(map[string]string{"title": title}))))
			req.Header.Set("Authorization", "Bearer demo-alpha-agent")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", etag(ticket.Version))
			resp, err := r.client.Do(req)
			if err != nil {
				results <- 0
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			results <- resp.StatusCode
		}(title)
	}
	close(start)
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for code := range results {
		counts[code]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("相同版本并发写必须一成一冲突：%v", counts)
	}
	if after := r.ticket(ticket.ID); after.Version != ticket.Version+1 {
		t.Fatal("版本不应推进两次")
	}
}
func TestHiddenT06Idempotency(t *testing.T) {
	r := newEvalRig(t)
	headers := map[string]string{"Idempotency-Key": "重复键"}
	in := map[string]string{"title": "第一次", "body": "正文"}
	status, b, _ := r.request("POST", "/api/tickets", "demo-alpha-requester", in, headers)
	if status != 201 {
		t.Fatal(status)
	}
	status, replay, h := r.request("POST", "/api/tickets", "demo-alpha-requester", in, headers)
	if status != 201 || string(b) != string(replay) || h.Get("Idempotency-Replayed") != "true" {
		t.Fatal("同一请求重试结果不一致")
	}
	status, _, _ = r.request("POST", "/api/tickets", "demo-alpha-requester", map[string]string{"title": "第二次不同正文"}, headers)
	if status != 409 {
		t.Fatalf("不同请求复用键必须冲突：%d", status)
	}
	var count int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM tickets`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("重复创建：%d", count)
	}
}
func TestHiddenT07Search(t *testing.T) {
	r := newEvalRig(t)
	r.create("进度100%_完成")
	r.create("进度100AB完成")
	for _, query := range []string{"%_", "100%_"} {
		status, b, _ := r.request("GET", "/api/tickets?q="+url.QueryEscape(query), "demo-alpha-agent", nil, nil)
		if status != 200 {
			t.Fatal(status)
		}
		var p ticketPage
		_ = json.Unmarshal(b, &p)
		if len(p.Items) != 1 || p.Items[0].Title != "进度100%_完成" {
			t.Fatalf("搜索应按字面匹配：%s", b)
		}
	}
}
func TestHiddenT08Migration(t *testing.T) {
	r := newEvalRig(t)
	ticket := r.create("迁移前数据")
	if status, _, _ := r.request("POST", "/__judge/migrate", "", nil, nil); status != 200 {
		t.Fatalf("重复迁移失败：%d", status)
	}
	if r.ticket(ticket.ID).Title != ticket.Title {
		t.Fatal("重复迁移破坏已有数据")
	}
	if _, err := r.pool.Exec(context.Background(), `UPDATE schema_migrations SET checksum='synthetic-tamper' WHERE name='001_schema.sql'`); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := r.request("POST", "/__judge/migrate", "", nil, nil); status == 200 {
		t.Fatal("已应用迁移被篡改应拒绝")
	}
}
func TestHiddenT09APICompatibility(t *testing.T) {
	r := newEvalRig(t)
	for _, tc := range []struct {
		path, token, code string
		status            int
	}{{"/api/tickets", "", "unauthorized", 401}, {"/api/tickets/999999", "demo-alpha-agent", "not_found", 404}, {"/api/tickets?limit=0", "demo-alpha-agent", "invalid_limit", 400}} {
		status, b, h := r.request("GET", tc.path, tc.token, nil, nil)
		var envelope struct {
			Error APIError `json:"error"`
		}
		if err := json.Unmarshal(b, &envelope); err != nil || status != tc.status || envelope.Error.Code != tc.code || envelope.Error.Message == "" || !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
			t.Fatalf("错误响应契约不兼容：%d %s", status, b)
		}
	}
}
func TestHiddenT10WebSafety(t *testing.T) {
	r := newEvalRig(t)
	payload := `<img src=x onerror=alert(1)>`
	status, body, headers := r.request("GET", "/?title="+url.QueryEscape(payload), "", nil, nil)
	if status != 200 || strings.Contains(string(body), payload) || !strings.Contains(string(body), "&lt;img") {
		t.Fatal("页面标题缺少自动转义")
	}
	csp := headers.Get("Content-Security-Policy")
	if !strings.Contains(csp, "object-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatal("安全响应头缺失")
	}
	status, script, _ := r.request("GET", "/static/app.js", "", nil, nil)
	if status != 200 || strings.Contains(string(script), ".innerHTML") || strings.Contains(string(script), "localStorage") {
		t.Fatal("不可信文本或令牌持久化路径不符合安全契约")
	}
}
