package issuedesk

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoleTransitions(t *testing.T) {
	for _, tc := range []struct {
		role, from, to string
		want           bool
	}{{"requester", "open", "cancelled", true}, {"requester", "open", "in_progress", false}, {"agent", "open", "in_progress", true}, {"agent", "in_progress", "resolved", true}, {"admin", "resolved", "closed", true}, {"admin", "closed", "open", false}, {"agent", "open", "closed", false}} {
		if got := allowedTransition(tc.role, tc.from, tc.to); got != tc.want {
			t.Errorf("%s %s -> %s = %v", tc.role, tc.from, tc.to, got)
		}
	}
}
func TestHealthAndHTML(t *testing.T) {
	app := NewApp(nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Fatalf("健康检查失败：%d", w.Code)
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/?title=%3Cscript%3Ealert(1)%3C%2Fscript%3E", nil))
	if strings.Contains(w.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("页面未转义标题")
	}
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("缺少内容安全策略")
	}
}
func TestStrictJSON(t *testing.T) {
	for _, body := range []string{`{"title":"甲","unknown":true}`, `{"title":"甲"} {}`, `[`} {
		r := httptest.NewRequest("POST", "/api/tickets", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if err := decode(httptest.NewRecorder(), r, &createInput{}); err == nil {
			t.Fatalf("应拒绝无效正文：%s", body)
		}
	}
}
func TestVersionContract(t *testing.T) {
	for _, raw := range []string{"", "1", `W/"1"`, `"0"`, `"-1"`, `"a"`} {
		r := httptest.NewRequest("PATCH", "/", nil)
		r.Header.Set("If-Match", raw)
		if _, err := matchVersion(r); err == nil {
			t.Errorf("应拒绝版本 %q", raw)
		}
	}
	r := httptest.NewRequest("PATCH", "/", nil)
	r.Header.Set("If-Match", `"12"`)
	if v, err := matchVersion(r); err != nil || v != 12 {
		t.Fatalf("有效版本解析失败：%d %v", v, err)
	}
}
func TestUnauthenticatedError(t *testing.T) {
	w := httptest.NewRecorder()
	NewApp(nil).ServeHTTP(w, httptest.NewRequest("GET", "/api/tickets", nil))
	if w.Code != 401 || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("未认证错误契约无效：%d %s", w.Code, w.Body.String())
	}
}
