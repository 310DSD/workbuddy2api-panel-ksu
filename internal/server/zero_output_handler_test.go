// zero_output_handler_test.go 零输出守卫在 handler 层的处置：
// 上游「200 + 收下请求却零输出」不罚号、换号重发（客户端无痕）；
// 全池都撞时回 502 zero_output_length + 可行动 hint（不是 no_healthy_account）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const zoSSeEmpty = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0,\"total_tokens\":10}}\n\n" +
	"data: [DONE]\n\n"

const zoSSeNormal = "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
	"data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// zoUpstream 造假上游：token "at-bad" 恒零输出，其余正常。calls 计上游请求数
// （证明「换号重发」真的发生过，而不是被拦在选号层）。
func zoUpstream(t *testing.T) (*upstream.Client, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(r.Header.Get("Authorization"), "at-bad") {
			_, _ = w.Write([]byte(zoSSeEmpty))
			return
		}
		_, _ = w.Write([]byte(zoSSeNormal))
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	return up, &calls
}

func zoHandler(t *testing.T, up *upstream.Client, uids ...string) (*Handler, map[string]*auth.Auth) {
	t.Helper()
	accts := make([]*auth.Auth, 0, len(uids))
	byUID := map[string]*auth.Auth{}
	for i, uid := range uids {
		token := "at-good-" + uid
		if i == 0 {
			token = "at-bad" // 第一个号固定零输出（选号顺序按 uid 字典序）
		}
		a := &auth.Auth{UID: uid, AccessToken: token, ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
		accts = append(accts, a)
		byUID[uid] = a
	}
	p := testPoolWith(accts...)
	return NewHandler(Config{Pool: p, Upstream: up}), byUID
}

// TestChatZeroOutputRotatesToHealthyAccount 流式：零输出号被换掉，客户端拿到正常
// 回复，且零输出号**没有被罚**（连败/冷却/失败计数都不动）。
func TestChatZeroOutputRotatesToHealthyAccount(t *testing.T) {
	up, calls := zoUpstream(t)
	h, _ := zoHandler(t, up, "u1", "u2")
	const body = `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("客户端应拿到第二个号的正常回复，实际 %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "zero_output") {
		t.Fatal("换号成功时客户端不应看到任何零输出痕迹（必须无痕）")
	}
	if n := *calls; n < 2 {
		t.Fatalf("上游请求数=%d want >=2（零输出后必须换号重发）", n)
	}
	if st, ok := h.cfg.Pool.Status("u1"); !ok || st.ErrTotal != 0 || st.Cooling {
		t.Fatalf("零输出不得罚账号（请求形态问题，非账号健康问题）：%+v", st)
	}
}

// TestChatZeroOutputAllAccountsReturns502 全池零输出：502 + zero_output_length +
// 可行动 hint，且不是 no_healthy_account（那会让客户端无脑重试）。
func TestChatZeroOutputAllAccountsReturns502(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(zoSSeEmpty))
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.ChatBaseCN = srv.URL

	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "u2", AccessToken: "at-2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	const body = `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body.String())
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
	if got.Error.Code != "zero_output_length" {
		t.Fatalf("code=%q want zero_output_length（全池零输出不是 no_healthy_account）", got.Error.Code)
	}
	if !strings.Contains(got.Error.GatewayHint, "max_tokens") {
		t.Fatalf("hint 应可行动（缩短输入/调大 max_tokens），实际 %q", got.Error.GatewayHint)
	}
	// 每个号各试一次（不因零输出提前止损，也不重复轰炸）。
	if calls > 3 {
		t.Fatalf("上游请求数=%d，超过轮转上限", calls)
	}
}

// TestChatZeroOutputNonStreamReturns502 非流式同判据：聚合体 finish=length + 全空
// 也是零输出（此前会被当成一次成功，客户端拿到空正文）。
func TestChatZeroOutputNonStreamReturns502(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(zoSSeEmpty))
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.ChatBaseCN = srv.URL

	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "u2", AccessToken: "at-2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	const body = `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "zero_output_length") {
		t.Fatalf("body 应含 zero_output_length，实际 %s", rec.Body.String())
	}
	if calls < 2 {
		t.Fatalf("非流式也应换号重发一次，上游请求数=%d", calls)
	}
}
