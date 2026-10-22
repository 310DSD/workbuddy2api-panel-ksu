// modelunavail_handler_test.go 整体不可用模型在 handler 层的两个出口：
// 请求在打上游之前被拒（400 model_unavailable），以及 /v1/models 不再列出它。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestChatRejectsModelUnavailableBeforeUpstream 整体不可用模型：在打上游之前就回
// 400 model_unavailable（省掉「每个号都白撞一次」），且上游请求数为 0。
func TestChatRejectsModelUnavailableBeforeUpstream(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "u2", AccessToken: "at-2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	p.BlockModelBackoff("u1", "ghost", upstream.ModelBlockReason)
	p.BlockModelBackoff("u2", "ghost", upstream.ModelBlockReason)
	// 消耗掉首次探针窗口：先让 pool 放行一次探针，再发真请求 → 本次应被拦。
	if _, blocked, probe := p.ModelUnavailableFor("cn", "ghost"); blocked || !probe {
		t.Fatal("前置失效：判定后首个调用应是探针（放行）")
	}
	p.MarkModelUnavailableProbe("cn", "ghost")

	var calls int64
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		atomic.AddInt64(&calls, 1)
		return 200, `{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	const body = `{"model":"cn:ghost","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error struct {
			Code        string `json:"code"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v (%s)", err, rec.Body.String())
	}
	if got.Error.Code != "model_unavailable" {
		t.Fatalf("code=%q want model_unavailable", got.Error.Code)
	}
	if !strings.Contains(got.Error.GatewayHint, "another model") {
		t.Errorf("hint 应指向换模型，实际 %q", got.Error.GatewayHint)
	}
	if n := atomic.LoadInt64(&calls); n != 0 {
		t.Fatalf("整体不可用模型必须在打上游之前拒掉，实际上游请求数=%d", n)
	}
}

// TestModelsListHidesModelUnavailable /v1/models 不列出整体不可用的模型：
// 列出它只会让客户端选到一个必然失败的模型。
func TestModelsListHidesModelUnavailable(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"ghost"},{"id":"good-model"}],"agents":[{"name":"cli","models":["ghost","good-model"]}]}}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "u2", AccessToken: "at-2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	p.BlockModelBackoff("u1", "ghost", upstream.ModelBlockReason)
	p.BlockModelBackoff("u2", "ghost", upstream.ModelBlockReason)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range resp.Data {
		id, _ := m["id"].(string)
		ids[id] = true
	}
	if ids["cn:ghost"] {
		t.Fatalf("整体不可用模型不应出现在 /v1/models 里：%v", ids)
	}
	if !ids["cn:good-model"] {
		t.Fatalf("其他模型必须照常列出：%v", ids)
	}
}
