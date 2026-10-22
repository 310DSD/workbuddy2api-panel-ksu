// zero_output_test.go 零输出守卫（WithZeroOutputGuard / errZeroOutput）：
// 「上游 200 + finish_reason=length + 正文/思考/工具全空」必须在**零字节写出**的
// 前提下被识别，才能换号重发而不惊动客户端。
package upstream

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// zeroOutputStream 线上实测的零输出形态：role 帧 → 空 delta 帧 → finish=length。
// 全程没有一个可见字节。
const zeroOutputStream = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0,\"total_tokens\":10}}\n\n" +
	"data: [DONE]\n\n"

const normalStream = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// TestZeroOutputGuardDetectsEmpty 守卫开启：零输出流返回 errZeroOutput，且
// **一个字节都没写出**（响应体为空）——这是「客户端无痕换号」的前提。
func TestZeroOutputGuardDetectsEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(zeroOutputStream), nil, WithZeroOutputGuard())
	if !IsZeroOutputError(err) {
		t.Fatalf("err=%v want errZeroOutput", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("零输出守卫必须在零字节写出下返回，实际写了 %d 字节：%q", rec.Body.Len(), rec.Body.String())
	}
	// 也不得写出 [DONE]：那会让客户端以为回合正常结束。
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatal("零输出流不得向客户端写 [DONE]")
	}
}

// TestZeroOutputGuardOffKeepsLegacy 未开守卫时行为与旧版一致：
// 壳帧照常透传、返回 nil（不给客户端换成 502 的理由）。
func TestZeroOutputGuardOffKeepsLegacy(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(zeroOutputStream), nil); err != nil {
		t.Fatalf("未开守卫时不应返回错误：%v", err)
	}
	if rec.Body.Len() == 0 || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("未开守卫时应照常透传壳帧与 [DONE]，实际 %q", rec.Body.String())
	}
}

// TestZeroOutputGuardNormalStreamUnchanged 正常流：守卫不改变任何透传字节
// （开着守卫与不开守卫的输出必须逐字节相同）。
func TestZeroOutputGuardNormalStreamUnchanged(t *testing.T) {
	off := httptest.NewRecorder()
	if err := StreamHint(off, strings.NewReader(normalStream), nil); err != nil {
		t.Fatalf("guard off: %v", err)
	}
	on := httptest.NewRecorder()
	if err := StreamHint(on, strings.NewReader(normalStream), nil, WithZeroOutputGuard()); err != nil {
		t.Fatalf("guard on: %v", err)
	}
	if off.Body.String() != on.Body.String() {
		t.Fatalf("守卫改变了正常流的透传字节：\n off=%q\n on =%q", off.Body.String(), on.Body.String())
	}
	if !strings.Contains(on.Body.String(), "你好") {
		t.Fatalf("正常流应含正文，实际 %q", on.Body.String())
	}
}

// TestZeroOutputGuardReasoningOnlyIsNotZero 只有思考、没有正文的流**不算**零输出：
// 客户端确实收到了 reasoning_content，回合有内容可消费（截断与否由既有逻辑管）。
func TestZeroOutputGuardReasoningOnlyIsNotZero(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"想一下\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(stream), nil, WithZeroOutputGuard()); err != nil {
		t.Fatalf("纯思考流不应判零输出：%v", err)
	}
	if !strings.Contains(rec.Body.String(), "想一下") {
		t.Fatalf("思考内容必须透传，实际 %q", rec.Body.String())
	}
}

// TestZeroOutputGuardErrorFrameStillPassesThrough 上游 error 帧优先：
// 有错误就交给既有错误路径（不判零输出，也不吞帧）。
func TestZeroOutputGuardErrorFrameStillPassesThrough(t *testing.T) {
	stream := "data: {\"error\":{\"message\":\"6004 rate limited\",\"code\":\"6004\"}}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	var seen string
	err := StreamHint(rec, strings.NewReader(stream), nil,
		WithZeroOutputGuard(), WithErrorFrameObserver(func(p string) { seen = p }))
	if err != nil {
		t.Fatalf("error 帧不应变成零输出错误：%v", err)
	}
	if !strings.Contains(seen, "6004") || !strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("error 帧必须透出（观察者=%q body=%q）", seen, rec.Body.String())
	}
}

// TestZeroOutputGuardEmptyStreamUnchanged 空流（0 帧）仍走既有 errEmptyStream，
// 且兜底 error 帧 + [DONE] 必须真正送达客户端（守卫不能把它们吞在缓冲里）。
func TestZeroOutputGuardEmptyStreamUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(""), nil, WithZeroOutputGuard())
	if !IsEmptyStreamError(err) {
		t.Fatalf("err=%v want errEmptyStream", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "empty upstream stream") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("空流兜底帧必须送达客户端，实际 %q", body)
	}
}

// TestIsZeroOutputResponse 非流式口径：finish=length 且正文/思考/工具全空才算；
// 有任何一项内容、或 finish 不是 length，都不算。
func TestIsZeroOutputResponse(t *testing.T) {
	choice := func(msg map[string]any, finish string) map[string]any {
		return map[string]any{"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}}
	}
	cases := []struct {
		name string
		resp map[string]any
		want bool
	}{
		{"空正文+length", choice(map[string]any{"role": "assistant", "content": ""}, "length"), true},
		{"空 content=null", choice(map[string]any{"role": "assistant"}, "length"), true},
		{"有正文", choice(map[string]any{"content": "hi"}, "length"), false},
		{"有思考", choice(map[string]any{"content": "", "reasoning_content": "想"}, "length"), false},
		{"有工具调用", choice(map[string]any{"content": "", "tool_calls": []any{map[string]any{"id": "x"}}}, "length"), false},
		// Aggregate 的真实形态是 []map[string]any：只认 []any 会把纯工具调用误判零输出。
		{"有工具调用（[]map 形态）", choice(map[string]any{"content": "", "tool_calls": []map[string]any{{"id": "x"}}}, "tool_calls"), false},
		// M6 对齐：空正文 + stop 同样是「客户端拿到空回复」，非流式也要判零输出。
		{"空正文但 finish=stop", choice(map[string]any{"content": ""}, "stop"), true},
		// content_filter 例外：重试必然同样被拦，不判零输出。
		{"空正文但被内容过滤", choice(map[string]any{"content": ""}, "content_filter"), false},
		{"无 choices", map[string]any{}, false},
	}
	for _, c := range cases {
		if got := IsZeroOutputResponse(c.resp); got != c.want {
			t.Errorf("%s: IsZeroOutputResponse=%v want %v", c.name, got, c.want)
		}
	}
	// 哨兵错误判定：IsZeroOutputError 只认自己。
	if IsZeroOutputError(errEmptyStream) || !errors.Is(errZeroOutput, errZeroOutput) {
		t.Fatal("IsZeroOutputError 判定异常")
	}
}

// TestZeroOutputGuardBufferCap 兜底上限：上游持续吐「不可见帧」时不得无限攒内存。
// 超过 zeroGuardPendingCap 后按「已有真实字节」提交，返回 nil（旧行为），
// 而不是继续缓冲到零输出判定、更不是 OOM。
func TestZeroOutputGuardBufferCap(t *testing.T) {
	frame := "data: {\"choices\":[{\"index\":0,\"delta\":{}}]}\n\n"
	n := zeroGuardPendingCap/len(frame) + 100
	var b strings.Builder
	b.Grow(n * len(frame))
	for i := 0; i < n; i++ {
		b.WriteString(frame)
	}
	b.WriteString("data: [DONE]\n\n")

	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(b.String()), nil, WithZeroOutputGuard())
	if err != nil {
		t.Fatalf("超限后应提交并返回 nil（旧行为），got %v", err)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("超限提交后客户端必须收到字节")
	}
}

// TestZeroOutputGuardNoFinishReason 有帧但既无可见输出、也没有 finish_reason
// （上游在收尾前断流）：与「空回复」同判据——客户端同样拿不到任何内容。
func TestZeroOutputGuardNoFinishReason(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"choices":[{"index":0,"delta":{}}]}

data: [DONE]

`
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(stream), nil, WithZeroOutputGuard())
	if !IsZeroOutputError(err) {
		t.Fatalf("err=%v want errZeroOutput（无 finish_reason 的空流同样是零输出）", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("零字节写出被破坏：%q", rec.Body.String())
	}
}

// TestZeroOutputGuardContentFilterNotZero content_filter 收尾的空内容**不**判零输出：
// 重试必然同样被拦，回放让客户端看到真实结果。
func TestZeroOutputGuardContentFilterNotZero(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}

data: [DONE]

`
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(stream), nil, WithZeroOutputGuard()); err != nil {
		t.Fatalf("content_filter 不应判零输出：%v", err)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("应正常收尾，实际 %q", rec.Body.String())
	}
}

// TestZeroOutputGuardCommentOnlyStream 只有注释行（无 data 帧）+ [DONE]：
// 视同空流（errEmptyStream），兜底 error 帧 + [DONE] 必须送达客户端（守卫不得吞掉）。
func TestZeroOutputGuardCommentOnlyStream(t *testing.T) {
	stream := ": keepalive\n\n: keepalive\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(stream), nil, WithZeroOutputGuard())
	if !IsEmptyStreamError(err) {
		t.Fatalf("err=%v want errEmptyStream", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "empty upstream stream") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("兜底帧必须送达，实际 %q", body)
	}
}
