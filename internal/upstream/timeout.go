// timeout.go 出站超时的分类（诊断 + 重试策略的单一事实来源）。
//
// 为什么需要分类：此前所有「超时」都归成一个 upstream_timeout，文案按「上游慢」
// 处理（停轮转、提示没有可用账号）。但移动端实测的 503 大多不是上游慢，而是
// **本机到上游的连接建立超时**：手机切网 / 休眠唤醒 / NAT 半死连接会让新拨号
// 卡到 dialTimeout（10s），错误恰好在上次输出后约 10s 出现。两者的处置完全不同：
//
//   - dial / TLS：请求根本没到达上游 → 换新连接重试是安全且有效的（不重复计费）；
//   - header（响应头超时）：请求已到上游、可能正在生成 → 重试有重复计费风险，
//     按「上游慢」处理（停轮转 + 提示调大 header_timeout / 换时间重试）；
//   - idle（流中空闲）：客户端已收到部分输出，不能重放，只能如实中断。
package upstream

import (
	"context"
	"errors"
	"net"
	"strings"
)

// TimeoutKind 出站超时的分类。
type TimeoutKind string

const (
	// TimeoutNone 不是超时（或 nil）。
	TimeoutNone TimeoutKind = ""
	// TimeoutDial TCP 拨号超时（含 DNS 解析 + 建连；手机切网/休眠后最常见）。
	TimeoutDial TimeoutKind = "dial"
	// TimeoutTLS TLS 握手超时（连接已建立但握手未完成）。
	TimeoutTLS TimeoutKind = "tls"
	// TimeoutHeader 响应头超时（请求已发出，上游迟迟不开始回包）。
	TimeoutHeader TimeoutKind = "header"
	// TimeoutIdle 流中空闲超时（idle watchdog 掐断，客户端仍在）。
	TimeoutIdle TimeoutKind = "idle"
	// TimeoutOther 其它超时（未知形态）。
	TimeoutOther TimeoutKind = "other"
)

// String 便于日志与 /status 展示。
func (k TimeoutKind) String() string { return string(k) }

// IsConnectTimeout 报告是否为「连接建立阶段」超时（dial/TLS）：
// 这类失败请求没有到达上游，重试安全（不会重复计费/重复生成）。
func (k TimeoutKind) IsConnectTimeout() bool {
	return k == TimeoutDial || k == TimeoutTLS
}

// ClassifyTimeout 把出站错误归类成 TimeoutKind（非超时返回 TimeoutNone）。
//
// 判定顺序：先确认「确实是超时」（net.Error.Timeout 或 context deadline）；
// 再看具体形态：
//   - *net.OpError{Op:"dial"}  → dial（DialContext 的 Timeout 覆盖 DNS + TCP）；
//   - TLS 握手超时              → tls；
//   - "timeout awaiting response headers" → header（ResponseHeaderTimeout）；
//   - context.Canceled / DeadlineExceeded → idle（本网关 idle watchdog 取消；
//     调用方需保证「客户端仍在」，否则那是客户端断连，不是超时）。
func ClassifyTimeout(err error) TimeoutKind {
	if err == nil {
		return TimeoutNone
	}
	var ne net.Error
	isNetTimeout := errors.As(err, &ne) && ne.Timeout()
	isCtx := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	if !isNetTimeout && !isCtx {
		return TimeoutNone
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "dial":
			return TimeoutDial
		}
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "tls handshake timeout"), strings.Contains(msg, "tls: handshake"):
		return TimeoutTLS
	case strings.Contains(msg, "timeout awaiting response headers"):
		return TimeoutHeader
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return TimeoutIdle
	}
	return TimeoutOther
}
