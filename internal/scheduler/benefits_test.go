// benefits_test.go 礼包 / 官方补偿的「先查后领」与查询失败回退。
package scheduler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// benefitsScheduler 造一个指向假上游的调度器。
func benefitsScheduler(t *testing.T, handler http.HandlerFunc) *Scheduler {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.BillingBaseCN = srv.URL
	up.ChatBaseCN = srv.URL
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	p.Add(a)
	return New(Config{Pool: p, Upstream: up})
}

// TestClaimBenefitsStatusGate 状态查询正常：只有 Pending 才发领取请求。
func TestClaimBenefitsStatusGate(t *testing.T) {
	var giftClaims, compClaims int64
	s := benefitsScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/billing/meter/check-gift-claimed":
			// 已领过 → 不领
			_, _ = w.Write([]byte(`{"code":0,"data":{"active":true,"claimed":true,"credit_num":1500}}`))
		case "/billing/meter/claim-gift":
			atomic.AddInt64(&giftClaims, 1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":1500}}`))
		case "/billing/meter/compensation-status":
			// 有待领 → 要领
			_, _ = w.Write([]byte(`{"code":0,"data":{"active":true,"claimed":false,"credit_num":500,"end_time":"2026-12-31 23:59:59"}}`))
		case "/billing/meter/claim-compensation":
			atomic.AddInt64(&compClaims, 1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":500}}`))
		default:
			http.NotFound(w, r)
		}
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	s.claimBenefits(a)
	if n := atomic.LoadInt64(&giftClaims); n != 0 {
		t.Fatalf("已领过的礼包不应再发领取请求，got %d", n)
	}
	if n := atomic.LoadInt64(&compClaims); n != 1 {
		t.Fatalf("待领补偿应发一次领取请求，got %d", n)
	}
}

// TestClaimBenefitsFallsBackOnStatusError 状态查询接口挂掉（上游改版/域差异）时
// **回退盲领一次**：宁多一行日志，也不能让用户静默丢掉礼物与补偿。
func TestClaimBenefitsFallsBackOnStatusError(t *testing.T) {
	var giftClaims, compClaims int64
	s := benefitsScheduler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/billing/meter/check-gift-claimed", "/billing/meter/compensation-status":
			http.Error(w, `{"code":500,"msg":"boom"}`, http.StatusInternalServerError)
		case "/billing/meter/claim-gift":
			atomic.AddInt64(&giftClaims, 1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":1500}}`))
		case "/billing/meter/claim-compensation":
			atomic.AddInt64(&compClaims, 1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":500}}`))
		default:
			http.NotFound(w, r)
		}
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"}
	s.claimBenefits(a)
	if n := atomic.LoadInt64(&giftClaims); n != 1 {
		t.Fatalf("状态查询失败应回退盲领一次，gift claims=%d", n)
	}
	if n := atomic.LoadInt64(&compClaims); n != 1 {
		t.Fatalf("状态查询失败应回退盲领一次，comp claims=%d", n)
	}
}
