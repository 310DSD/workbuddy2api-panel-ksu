// timeout_handler_test.go 手机端 503 upstream_timeout 的文案与可观测性：
// 连接层超时（dial）必须给出「网络路径」型 hint，而不是「没有可用账号」。
package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// hpTimeoutErr 满足 net.Error 且 Timeout()=true。
type hpTimeoutErr struct{ msg string }

func (e hpTimeoutErr) Error() string   { return e.msg }
func (e hpTimeoutErr) Timeout() bool   { return true }
func (e hpTimeoutErr) Temporary() bool { return true }

// timeoutUpstream 所有出站请求都返回指定超时错误。
func timeoutUpstream(t *testing.T, err error) (*upstream.Client, *int64) {
	t.Helper()
	var calls int64
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt64(&calls, 1)
		return nil, err
	})
	return &upstream.Client{
		ChatHTTP:   &http.Client{Transport: rt},
		HTTP:       &http.Client{Transport: rt},
		ChatBaseCN: "https://chat.example",
	}, &calls
}

// TestChatDialTimeoutMessageAndHint 连接层超时：503 + upstream_timeout，
// message 说清是 dial、hint 指向网络而不是账号池。
func TestChatDialTimeoutMessageAndHint(t *testing.T) {
	up, calls := timeoutUpstream(t, &net.OpError{Op: "dial", Net: "tcp", Err: hpTimeoutErr{"i/o timeout"}})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	const body = `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v (%s)", err, rec.Body.String())
	}
	if got.Error.Code != "upstream_timeout" {
		t.Fatalf("code=%q want upstream_timeout", got.Error.Code)
	}
	if !strings.Contains(got.Error.Message, "(dial)") || !strings.Contains(got.Error.Message, "connection timeout") {
		t.Errorf("message 应说明是连接超时：%q", got.Error.Message)
	}
	if !strings.Contains(got.Error.GatewayHint, "network_timeout") {
		t.Errorf("hint 应是网络型（network_timeout），实际 %q", got.Error.GatewayHint)
	}
	if strings.Contains(got.Error.GatewayHint, "no healthy account") {
		t.Errorf("连接超时不得套用「没有可用账号」文案：%q", got.Error.GatewayHint)
	}
	// upstream 内部对 dial 超时会重试一次：1 个账号 × 2 次尝试。
	if n := atomic.LoadInt64(calls); n != 2 {
		t.Errorf("上游尝试次数=%d want 2（连接层超时重试一次）", n)
	}
	// /status 分类计数可查。
	srec := httptest.NewRecorder()
	h.ServeHTTP(srec, httptest.NewRequest("GET", "/status", nil))
	var status struct {
		Timeouts struct {
			Counts map[string]int64 `json:"counts"`
			Last   struct {
				Kind string `json:"kind"`
			} `json:"last"`
			ConnectRetries int64 `json:"connect_retries"`
		} `json:"upstream_timeouts"`
	}
	if err := json.Unmarshal(srec.Body.Bytes(), &status); err != nil {
		t.Fatalf("status 不是 JSON：%v", err)
	}
	if status.Timeouts.Counts["dial"] != 1 {
		t.Errorf("status.upstream_timeouts.counts.dial=%d want 1（%v）", status.Timeouts.Counts["dial"], status.Timeouts.Counts)
	}
	if status.Timeouts.Last.Kind != "dial" {
		t.Errorf("last.kind=%q want dial", status.Timeouts.Last.Kind)
	}
	if status.Timeouts.ConnectRetries != 1 {
		t.Errorf("connect_retries=%d want 1", status.Timeouts.ConnectRetries)
	}
}

// TestChatHeaderTimeoutHint header 超时：hint 指向 header_timeout_seconds，且不重试。
func TestChatHeaderTimeoutHint(t *testing.T) {
	up, calls := timeoutUpstream(t, hpTimeoutErr{"net/http: timeout awaiting response headers"})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rec.Code)
	}
	var got struct {
		Error struct {
			GatewayHint string `json:"gateway_hint"`
			Message     string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Error.GatewayHint, "upstream_slow_start") {
		t.Errorf("hint=%q want upstream_slow_start", got.Error.GatewayHint)
	}
	if strings.Contains(got.Error.Message, "(dial)") {
		t.Errorf("header 超时不应被描述成 dial：%q", got.Error.Message)
	}
	if n := atomic.LoadInt64(calls); n != 1 {
		t.Errorf("尝试次数=%d want 1（header 超时不重试）", n)
	}
}

// 编译期守卫：确保 timeout.go 的 message/hint 对每个 kind 都有输出（空串会让
// 客户端只看到 code 没有解释）。
func TestUpstreamTimeoutTexts(t *testing.T) {
	for _, k := range []upstream.TimeoutKind{
		upstream.TimeoutDial, upstream.TimeoutTLS, upstream.TimeoutHeader,
		upstream.TimeoutIdle, upstream.TimeoutOther,
	} {
		if upstreamTimeoutMessage(k, 0) == "" || upstreamTimeoutHint(k) == "" {
			t.Errorf("%v 的 message/hint 不能为空", k)
		}
	}
}
