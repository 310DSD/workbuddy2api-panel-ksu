// compensation_test.go（panel）官方补偿端点：先查后领、无待领不发起领取、
// 查询失败逐行显形、单号/全量两条路径。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// compPanel 造面板：u1 有待领补偿、u2 已领、u3 查询失败（401）。
// claimCalls 记录真正打到 /billing/meter/claim-compensation 的次数——
// 「无待领不得发起领取」这个契约只能靠它证明。
func compPanel(t *testing.T) (*Panel, *int64) {
	t.Helper()
	var claimCalls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 账号按 Authorization 头区分（client 会把 access token 放进头发出去）。
		token := r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/billing/meter/compensation-status"):
			switch {
			case strings.Contains(token, "at-3"):
				http.Error(w, `{"code":401,"msg":"unauthorized"}`, http.StatusUnauthorized)
			case strings.Contains(token, "at-2"):
				_, _ = w.Write([]byte(`{"code":0,"data":{"active":true,"claimed":true,"credit_num":500}}`))
			default:
				_, _ = w.Write([]byte(`{"code":0,"data":{"active":true,"claimed":false,"credit_num":500,"end_time":"2026-10-22 23:59:59"}}`))
			}
		case strings.HasSuffix(r.URL.Path, "/billing/meter/claim-compensation"):
			atomic.AddInt64(&claimCalls, 1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":500}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	for _, a := range []*auth.Auth{
		{UID: "u1", AccessToken: "at-1", Domain: "www.codebuddy.cn", Nickname: "一号"},
		{UID: "u2", AccessToken: "at-2", Domain: "www.codebuddy.cn", Nickname: "二号"},
		{UID: "u3", AccessToken: "at-3", Domain: "www.codebuddy.cn", Nickname: "三号"},
	} {
		p.Add(a)
	}
	return New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up}), &claimCalls
}

func doPanel(t *testing.T, p *Panel, method, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	var body map[string]any
	if b := rec.Body.Bytes(); len(b) > 0 {
		_ = json.Unmarshal(b, &body)
	}
	return rec.Code, body
}

// TestCompensationListEndpoint 逐号查状态：可领 / 已领 / 查询失败各占一行，
// 汇总 pending 与 failed——「查不到」不得显示成「没有」。
func TestCompensationListEndpoint(t *testing.T) {
	p, _ := compPanel(t)
	code, body := doPanel(t, p, "GET", "/panel/api/compensation")
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%v", code, body)
	}
	rows, _ := body["accounts"].([]any)
	if len(rows) != 3 {
		t.Fatalf("账号行数=%d want 3", len(rows))
	}
	if got := body["pending"].(float64); got != 1 {
		t.Errorf("pending=%v want 1（只有 u1 可领）", got)
	}
	if got := body["failed"].(float64); got != 1 {
		t.Errorf("failed=%v want 1（u3 查询失败）", got)
	}
	for _, r := range rows {
		m := r.(map[string]any)
		switch m["uid"] {
		case "u1":
			if m["pending"] != true || m["credit_num"].(float64) != 500 {
				t.Errorf("u1=%v want pending/500", m)
			}
			if m["end_time"] != "2026-10-22 23:59:59" {
				t.Errorf("u1 end_time=%v", m["end_time"])
			}
			if v, ok := m["seconds_left"].(float64); !ok || v <= 0 {
				t.Errorf("u1 缺 seconds_left 或非正：%v", m["seconds_left"])
			}
		case "u2":
			if m["pending"] != false || m["claimed"] != true {
				t.Errorf("u2=%v want 已领不待领", m)
			}
		case "u3":
			if m["error"] == nil || m["error"] == "" {
				t.Errorf("u3 应带 error（查询失败照样占一行），got %v", m)
			}
		}
	}
}

// TestCompensationClaimGatesOnStatus 单号领取必须「先查后领」：
// 有待领 → 真发领取；已领 → 不发起领取（claimCalls 不增长）。
func TestCompensationClaimGatesOnStatus(t *testing.T) {
	p, claimCalls := compPanel(t)

	code, body := doPanel(t, p, "POST", "/panel/api/accounts/u1/compensation")
	if code != http.StatusOK || body["claimed"] != true {
		t.Fatalf("u1 领取 code=%d body=%v", code, body)
	}
	if got := body["credit"].(float64); got != 500 {
		t.Errorf("u1 credit=%v want 500", got)
	}
	if n := *claimCalls; n != 1 {
		t.Fatalf("领取请求数=%d want 1", n)
	}

	// 已领过：返回 claimed:false + reason，且**不得**发起领取。
	code, body = doPanel(t, p, "POST", "/panel/api/accounts/u2/compensation")
	if code != http.StatusOK || body["claimed"] != false {
		t.Fatalf("u2 code=%d body=%v", code, body)
	}
	if body["reason"] != "already_claimed" {
		t.Errorf("u2 reason=%v want already_claimed", body["reason"])
	}
	if n := *claimCalls; n != 1 {
		t.Fatalf("已领过的号不应发起领取，领取请求数=%d want 1", n)
	}

	// 未知账号 404。
	code, _ = doPanel(t, p, "POST", "/panel/api/accounts/nope/compensation")
	if code != http.StatusNotFound {
		t.Fatalf("未知 uid code=%d want 404", code)
	}
}

// TestCompensationClaimAllSkipsAndReports 全量领取：只领待领的，逐条返回结果。
func TestCompensationClaimAllSkipsAndReports(t *testing.T) {
	p, claimCalls := compPanel(t)
	code, body := doPanel(t, p, "POST", "/panel/api/compensation_all")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	if body["claimed"].(float64) != 1 {
		t.Errorf("claimed=%v want 1", body["claimed"])
	}
	if body["skipped"].(float64) != 1 {
		t.Errorf("skipped=%v want 1（u2 已领）", body["skipped"])
	}
	if body["failed"].(float64) != 1 {
		t.Errorf("failed=%v want 1（u3 查询失败）", body["failed"])
	}
	if n := *claimCalls; n != 1 {
		t.Errorf("领取请求数=%d want 1", n)
	}
}
