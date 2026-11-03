// timeout.go 出站超时的可观测性与对外文案（与 upstream/ClassifyTimeout 配套）。
//
// 背景：手机端（KernelSU 模块）偶发 503 upstream_timeout，旧文案固定写
// 「another account would hit the same slow upstream / no healthy account」——
// 但实测多数是**连接层超时**（dial 10s，且恰好在上次输出后约 10s 出现），
// 处置方向完全不同：不是账号池没号，而是本机网络路径/切网/休眠唤醒。
//
// 这里集中：① 进程内按 kind 计数（/status 可查）；② 按 kind 生成 message；
// ③ 按 kind 生成可行动 hint。
package server

import (
	"fmt"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// timeoutStats 出站超时分类计数（进程内，重启清零）。
type timeoutStats struct {
	mu       sync.Mutex
	counts   map[upstream.TimeoutKind]int64
	lastKind upstream.TimeoutKind
	lastAt   time.Time
	lastCost time.Duration
	lastErr  string
}

// note 记录一次超时（handler 在 break 之前调用）。
func (t *timeoutStats) note(kind upstream.TimeoutKind, elapsed time.Duration, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil {
		t.counts = map[upstream.TimeoutKind]int64{}
	}
	if kind == upstream.TimeoutNone {
		kind = upstream.TimeoutOther
	}
	t.counts[kind]++
	t.lastKind = kind
	t.lastAt = time.Now()
	t.lastCost = elapsed
	t.lastErr = logfmt.Truncate(fmt.Sprint(err), 200)
}

// last 返回最近一次超时的分类与耗时。
func (t *timeoutStats) last() (upstream.TimeoutKind, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastKind, t.lastCost
}

// snapshot 生成 /status.upstream_timeouts（nil Client 时省略连接层重试数）。
func (t *timeoutStats) snapshot(c *upstream.Client) map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	counts := map[string]int64{
		string(upstream.TimeoutDial):   0,
		string(upstream.TimeoutTLS):    0,
		string(upstream.TimeoutHeader): 0,
		string(upstream.TimeoutIdle):   0,
		string(upstream.TimeoutOther):  0,
	}
	for k, v := range t.counts {
		counts[string(k)] = v
	}
	out := map[string]any{"counts": counts}
	if !t.lastAt.IsZero() {
		out["last"] = map[string]any{
			"kind":       string(t.lastKind),
			"elapsed_ms": t.lastCost.Milliseconds(),
			"at":         t.lastAt,
			"error":      t.lastErr,
		}
	}
	if c != nil {
		out["connect_retries"] = c.TransportRetries()
	}
	return out
}

// upstreamTimeoutMessage 末端 error.message：按超时分类说清「卡在哪一段」。
// 保留上游原文的诉求在这里让位于可读性：超时错误原文是 Go 的 url.Error
// （含完整 URL/内部细节），对客户端没有操作价值。
func upstreamTimeoutMessage(kind upstream.TimeoutKind, elapsed time.Duration) string {
	d := elapsed.Round(100 * time.Millisecond)
	switch kind {
	case upstream.TimeoutDial:
		return fmt.Sprintf("upstream connection timeout after %s (dial): TCP connection to upstream could not be established; "+
			"this is a local network path problem (mobile handover / sleep / bad DNS) and not an account problem — the gateway already retried once on a fresh connection", d)
	case upstream.TimeoutTLS:
		return fmt.Sprintf("upstream TLS handshake timeout after %s: TCP connected but TLS did not complete; check network or proxy, then retry", d)
	case upstream.TimeoutHeader:
		return fmt.Sprintf("upstream response timeout after %s: request was sent but upstream did not start responding (header_timeout_seconds); retry later or raise the limit", d)
	case upstream.TimeoutIdle:
		return fmt.Sprintf("upstream stream stalled for %s: connection was idle mid-stream and was cut; retry", d)
	}
	return fmt.Sprintf("upstream timed out after %s: retry later", d)
}

// upstreamTimeoutHint 末端 gateway_hint：按分类给可执行动作。
// 关键：**不能**再用 no_healthy_account 的文案——手机端实测会被误读成「没号了」。
func upstreamTimeoutHint(kind upstream.TimeoutKind) string {
	switch kind {
	case upstream.TimeoutDial, upstream.TimeoutTLS:
		return "network_timeout: TCP/TLS to upstream timed out — typical on mobile handover or after the phone sleeps; retry usually succeeds. " +
			"If it persists: check DNS/proxy, switch Wi-Fi/cellular, or see /status.upstream_timeouts (counts by kind)."
	case upstream.TimeoutHeader:
		return "upstream_slow_start: no response headers within upstream.header_timeout_seconds (default 120s); " +
			"retry later, or raise upstream.header_timeout_seconds if this happens on long prompts."
	case upstream.TimeoutIdle:
		return "upstream_stalled: the SSE stream stayed idle beyond upstream.idle_timeout_seconds; retry, " +
			"or raise upstream.idle_timeout_seconds if the model legitimately thinks for long."
	}
	return "upstream_timeout: retry later; /status.upstream_timeouts shows the breakdown by kind (dial/tls/header/idle)."
}
