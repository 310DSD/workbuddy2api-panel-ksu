// timeout_test.go 出站超时分类与「连接层超时换新连接重试」的回归。
//
// 手机端实测：503 upstream_timeout 大多发生在「上次输出后约 10s」——正是旧
// dialTimeout 的值，说明是 TCP 拨号超时（切网/休眠后新连接建立失败），
// 不是上游回包慢。这里钉住三件事：
//  1. 能区分 dial / TLS / header / idle；
//  2. dial/TLS 超时会**换新连接重试一次**（请求没到上游，重试不重复计费）；
//  3. header 超时**不**重试（请求可能已到上游，重试有重复生成风险）。
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// timeoutErr 满足 net.Error 且 Timeout()=true。
type timeoutErr struct{ msg string }

func (e timeoutErr) Error() string   { return e.msg }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return true }

// TestClassifyTimeout 分类表。
func TestClassifyTimeout(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{"i/o timeout"}}
	tlsErr := timeoutErr{"net/http: TLS handshake timeout"}
	headerErr := timeoutErr{"net/http: timeout awaiting response headers"}

	cases := []struct {
		name string
		err  error
		want TimeoutKind
	}{
		{"nil", nil, TimeoutNone},
		{"dial", dialErr, TimeoutDial},
		{"dial 包装", fmt.Errorf("post: %w", dialErr), TimeoutDial},
		{"tls", tlsErr, TimeoutTLS},
		{"header", headerErr, TimeoutHeader},
		{"context canceled（idle watchdog）", context.Canceled, TimeoutIdle},
		{"context deadline", context.DeadlineExceeded, TimeoutIdle},
		{"普通错误", errors.New("connection refused"), TimeoutNone},
	}
	for _, c := range cases {
		if got := ClassifyTimeout(c.err); got != c.want {
			t.Errorf("%s: ClassifyTimeout=%v want %v", c.name, got, c.want)
		}
	}
	// 连接层判定（决定是否重试）。
	if !TimeoutDial.IsConnectTimeout() || !TimeoutTLS.IsConnectTimeout() {
		t.Fatal("dial/tls 必须视为可重试的连接层超时")
	}
	for _, k := range []TimeoutKind{TimeoutHeader, TimeoutIdle, TimeoutOther, TimeoutNone} {
		if k.IsConnectTimeout() {
			t.Errorf("%v 不应被视为连接层超时（重试有重复生成风险）", k)
		}
	}
}

// retryClient 造一个 ChatHTTPh 指向自定义 Transport 的 Client。
func retryClient(fn rtFunc) *Client {
	return &Client{
		ChatHTTP:   &http.Client{Transport: fn},
		ChatBaseCN: "https://chat.example",
	}
}

// TestChatStreamRetriesConnectTimeout dial 超时后换新连接重试一次并成功。
func TestChatStreamRetriesConnectTimeout(t *testing.T) {
	var calls int64
	c := retryClient(func(r *http.Request) (*http.Response, error) {
		if atomic.AddInt64(&calls, 1) == 1 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{"i/o timeout"}}
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}
	rc, status, _, err := c.ChatStreamContext(context.Background(), a, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("重试后应成功：status=%d err=%v", status, err)
	}
	rc.Close()
	if n := atomic.LoadInt64(&calls); n != 2 {
		t.Fatalf("尝试次数=%d want 2（dial 超时后重试一次）", n)
	}
	if got := c.TransportRetries(); got != 1 {
		t.Fatalf("TransportRetries()=%d want 1", got)
	}
}

// TestChatStreamDoesNotRetryHeaderTimeout header 超时不重试：请求已发出，
// 重试可能让上游重复生成/重复计费。
func TestChatStreamDoesNotRetryHeaderTimeout(t *testing.T) {
	var calls int64
	c := retryClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt64(&calls, 1)
		return nil, timeoutErr{"net/http: timeout awaiting response headers"}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}
	_, _, _, err := c.ChatStreamContext(context.Background(), a, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{})
	if err == nil {
		t.Fatal("header 超时应返回错误")
	}
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Fatalf("尝试次数=%d want 1（header 超时不得重试）", n)
	}
	if got := c.TransportRetries(); got != 0 {
		t.Fatalf("TransportRetries()=%d want 0", got)
	}
}

// TestChatStreamNoRetryWhenContextCanceled 客户端已断连：不重试。
func TestChatStreamNoRetryWhenContextCanceled(t *testing.T) {
	var calls int64
	c := retryClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt64(&calls, 1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{"i/o timeout"}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}
	if _, _, _, err := c.ChatStreamContext(ctx, a, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{}); err == nil {
		t.Fatal("ctx 已取消时应返回错误")
	}
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Fatalf("尝试次数=%d want 1（客户端已走，不重试）", n)
	}
}
