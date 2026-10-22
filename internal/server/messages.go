// messages.go Anthropic Messages 协议入站（POST /v1/messages、POST /messages）：
// 把 Messages 请求翻成 chat.completions 交给既有 chatCompletions，再把它写出的
// SSE/JSON 翻回 Anthropic 的 message / 事件流。
//
// 与 responses.go 同一条思路：适配器只做**协议翻译**，选号/轮转/冷却/熔断/提示词/
// 指纹/DSML 还原/零输出守卫全部复用 chat 管道。
//
// 覆盖范围（对齐 Claude Code 的实际用法）：
//   - system（字符串或 blocks）→ system 消息；messages 的 text/image/tool_use/
//     tool_result/thinking 块逐类翻译（thinking 只吞不喂：上游 reasoning 无 signature，
//     回传会让上游 400）
//   - tools[].input_schema → chat 的 function.parameters；tool_choice 三种形态
//   - max_tokens / temperature / top_p / stop_sequences / stream
//   - 非流式：chat.completion → Anthropic message（content 块含 thinking/text/tool_use）
//   - 流式：message_start / content_block_start / *_delta / content_block_stop /
//     message_delta / message_stop（含 input_json_delta 拼工具参数）
//
// 鉴权：Anthropic 客户端用 x-api-key（Claude Code）或 Authorization: Bearer
// （部分代理配置），两者都接受，与网关 api_key 同口径。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// messagesWebsocketUnsupported 回应意外升级探测（Anthropic 无 WS 协议，保持显式）。
func (h *Handler) messagesWebsocketUnsupported(w http.ResponseWriter, r *http.Request) {
	writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error",
		"websocket transport unsupported; use POST /v1/messages")
}

// messages 处理 POST /v1/messages 与 POST /messages。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	// 与 chat 主通道同口径：不做网关侧请求体上限（大上下文/多图由上游裁决）。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	chat, meta, err := messagesToChat(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chat))
	r2.ContentLength = int64(len(chat))
	rw := newAnthropicWriter(w, meta.stream, modelBare(meta.model))
	defer rw.finish()
	h.chatCompletions(rw, r2)
}

// messagesMeta 需要在回程复用的请求信息。
type messagesMeta struct {
	model  string
	stream bool
}

// messagesToChat 把 Anthropic Messages 请求翻译成 chat.completions 请求。
func messagesToChat(src []byte) ([]byte, *messagesMeta, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON: %w", err)
	}
	meta := &messagesMeta{}
	out := map[string]any{}
	for _, k := range []string{"model", "stream", "temperature", "top_p", "max_tokens"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	meta.model, _ = out["model"].(string)
	meta.stream, _ = out["stream"].(bool)

	// stop_sequences → stop；top_k 上游无对应语义，忽略。
	if stops, ok := obj["stop_sequences"].([]any); ok && len(stops) > 0 {
		out["stop"] = stops
	}
	// thinking 档位：{"type":"enabled","budget_tokens":N} → reasoning_effort，
	// 按预算档折算（上游只有档位，没有绝对 token 预算）。
	if th, ok := obj["thinking"].(map[string]any); ok {
		if t, _ := th["type"].(string); t == "enabled" {
			if e := effortFromThinkingBudget(th["budget_tokens"]); e != "" {
				out["reasoning_effort"] = e
			}
		}
	}
	if e, ok := obj["reasoning_effort"].(string); ok && e != "" {
		out["reasoning_effort"] = e
	}

	msgs := make([]any, 0, 16)
	// system 可能是字符串，也可能是 blocks 数组。
	switch s := obj["system"].(type) {
	case string:
		if strings.TrimSpace(s) != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": s})
		}
	case []any:
		if txt := anthropicBlocksText(s); txt != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": txt})
		}
	}
	rawMsgs, _ := obj["messages"].([]any)
	msgs = append(msgs, anthropicMessagesToChat(rawMsgs)...)
	out["messages"] = msgs

	if tools, ok := obj["tools"].([]any); ok {
		if ct := anthropicToolsToChat(tools); len(ct) > 0 {
			out["tools"] = ct
		}
	}
	if tc, ok := obj["tool_choice"]; ok && tc != nil {
		if v := anthropicToolChoiceToChat(tc); v != nil {
			out["tool_choice"] = v
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return raw, meta, nil
}

// effortFromThinkingBudget 把 thinking.budget_tokens 折算成上游档位。
// 档位来自上游目录（low/medium/high…），这里按常见预算切分；未知预算不下发档位
// （让上游按模型默认走，而不是猜一个）。
func effortFromThinkingBudget(v any) string {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return ""
	}
	switch {
	case f <= 1024:
		return "low"
	case f <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// anthropicBlocksText 把 blocks 里的 text 拼成单串（system / tool_result 用）。
func anthropicBlocksText(blocks []any) string {
	var b strings.Builder
	for _, bi := range blocks {
		m, ok := bi.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "text" {
			s, _ := m["text"].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}

// anthropicMessagesToChat 逐条翻译 messages（role + content blocks）。
//
// 块级语义：
//   - text        → 正文（多个 text 块拼接）
//   - image       → chat 的 image_url part（base64 源转 data URI）
//   - tool_use    → assistant.tool_calls（同一轮的多个调用合并到同一条 assistant）
//   - tool_result → role=tool 消息（tool_call_id 对应 tool_use.id）
//   - thinking / redacted_thinking → 丢弃（上游 reasoning 无 signature，回传会 400）
func anthropicMessagesToChat(raw []any) []any {
	out := make([]any, 0, len(raw))
	for _, mi := range raw {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		switch c := m["content"].(type) {
		case string:
			out = append(out, map[string]any{"role": role, "content": c})
		case []any:
			out = append(out, anthropicBlocksToMessages(role, c)...)
		default:
			out = append(out, map[string]any{"role": role, "content": ""})
		}
	}
	return out
}

// anthropicBlocksToMessages 把一条消息的 blocks 展开成可能的多条 chat 消息
// （tool_use → assistant；tool_result → tool；其余 → 文本/多模态）。
func anthropicBlocksToMessages(role string, blocks []any) []any {
	var (
		texts     strings.Builder
		parts     []any
		toolCalls []any
		out       []any
	)
	flushText := func() {
		if texts.Len() == 0 && len(parts) == 0 {
			return
		}
		content := any(texts.String())
		if len(parts) > 0 {
			if texts.Len() > 0 {
				parts = append([]any{map[string]any{"type": "text", "text": texts.String()}}, parts...)
			}
			content = parts
		}
		out = append(out, map[string]any{"role": role, "content": content})
		texts.Reset()
		parts = nil
	}
	for _, bi := range blocks {
		b, ok := bi.(map[string]any)
		if !ok {
			continue
		}
		switch t, _ := b["type"].(string); t {
		case "text":
			s, _ := b["text"].(string)
			texts.WriteString(s)
		case "image":
			if url := anthropicImageURL(b); url != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		case "tool_use":
			flushText()
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			args := stringifyArgs(b["input"])
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
		case "tool_result":
			flushText()
			if len(toolCalls) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls})
				toolCalls = nil
			}
			id, _ := b["tool_use_id"].(string)
			out = append(out, map[string]any{
				"role": "tool", "tool_call_id": id, "content": anthropicToolResultText(b),
			})
		case "thinking", "redacted_thinking":
			// 不喂回上游（见函数注释）。
			continue
		}
	}
	flushText()
	if len(toolCalls) > 0 {
		// assistant 的「正文 + tool_use」必须落在**同一条**消息里：拆成两条
		// consecutive assistant 会被上游拒绝（或让模型看不到自己说过的话）。
		// OpenAI 的 assistant 消息本就允许 content 与 tool_calls 并存。
		if role == "assistant" && len(out) > 0 {
			if last, ok := out[len(out)-1].(map[string]any); ok &&
				last["role"] == "assistant" && last["tool_calls"] == nil {
				last["tool_calls"] = toolCalls
				return out
			}
		}
		out = append(out, map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls})
	}
	if len(out) == 0 {
		// 全是被丢弃的块（如只有 thinking）：保留一条空消息，避免上游看到「空对话」
		// 却不知道客户端确实发过东西。
		out = append(out, map[string]any{"role": role, "content": ""})
	}
	return out
}

// anthropicImageURL 把 image 块转成 chat 可用的 URL（base64 → data URI）。
func anthropicImageURL(b map[string]any) string {
	src, _ := b["source"].(map[string]any)
	if src == nil {
		return ""
	}
	switch t, _ := src["type"].(string); t {
	case "base64":
		mt, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if data == "" {
			return ""
		}
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + data
	case "url":
		u, _ := src["url"].(string)
		return u
	}
	return ""
}

// anthropicToolResultText tool_result 的内容提取（字符串或 text blocks）。
func anthropicToolResultText(b map[string]any) string {
	switch c := b["content"].(type) {
	case string:
		return c
	case []any:
		return anthropicBlocksText(c)
	default:
		return stringifyArgs(b["content"])
	}
}

// anthropicToolsToChat tools[].input_schema → chat 的 function.parameters。
func anthropicToolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, ti := range tools {
		t, ok := ti.(map[string]any)
		if !ok {
			continue
		}
		// 服务端内置工具（web_search_20250305 等，type 非 custom/空）本网关不执行，丢弃。
		if ty, _ := t["type"].(string); ty != "" && ty != "custom" {
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d, ok := t["description"].(string); ok && d != "" {
			fn["description"] = d
		}
		if s, ok := t["input_schema"]; ok && s != nil {
			fn["parameters"] = s
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// anthropicToolChoiceToChat {"type":"auto"|"any"|"none"|"tool","name"} → chat 形态。
func anthropicToolChoiceToChat(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	switch t, _ := m["type"].(string); t {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name, _ := m["name"].(string)
		if name == "" {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 回程：chat 流/JSON → Anthropic message / 事件流
// ---------------------------------------------------------------------------

// anthBlock 一个正在流式输出的 Anthropic content block。
type anthBlock struct {
	idx    int
	typ    string // text | thinking | tool_use
	id     string
	name   string
	buf    strings.Builder // text / thinking 正文 或 tool_use 的参数 JSON 增量
	opened bool
	closed bool
}

// anthState 单次 Messages 请求的回程状态。
type anthState struct {
	id        string
	model     string
	started   bool
	nextIdx   int
	thinking  *anthBlock
	text      *anthBlock
	tools     map[int]*anthBlock
	toolOrder []int
	content   []map[string]any // 已收尾的 content blocks
	stop      string           // Anthropic stop_reason
	usage     map[string]any
	done      bool
	failed    bool
}

// anthropicWriter 拦截 chatCompletions 的写出并翻译成 Anthropic 协议。
type anthropicWriter struct {
	inner  http.ResponseWriter
	stream bool
	model  string
	status int
	body   bytes.Buffer
	x      *anthState
}

func newAnthropicWriter(w http.ResponseWriter, stream bool, model string) *anthropicWriter {
	return &anthropicWriter{
		inner: w, stream: stream, model: model,
		x: &anthState{
			id:    "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36),
			model: model,
			tools: map[int]*anthBlock{},
		},
	}
}

func (w *anthropicWriter) Header() http.Header  { return w.inner.Header() }
func (w *anthropicWriter) WriteHeader(code int) { w.status = code }
func (w *anthropicWriter) Flush() {
	if f, ok := w.inner.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *anthropicWriter) Write(p []byte) (int, error) {
	if !w.stream {
		w.body.Write(p)
		return len(p), nil
	}
	w.body.Write(p)
	w.drainFrames()
	return len(p), nil
}

func (w *anthropicWriter) drainFrames() {
	for {
		b := w.body.Bytes()
		i := bytes.Index(b, []byte("\n\n"))
		if i < 0 {
			return
		}
		frame := string(b[:i])
		w.body.Next(i + 2)
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimRight(line, "\r")
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				return
			}
			w.handleChunk(payload)
		}
	}
}

// handleChunk 翻译单个 chat chunk 成 Anthropic 事件。
func (w *anthropicWriter) handleChunk(payload string) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return
	}
	if e, ok := obj["error"]; ok {
		w.emitError(e)
		return
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		w.x.usage = u
	}
	chs, _ := obj["choices"].([]any)
	if len(chs) == 0 {
		return
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return
	}
	delta, _ := c["delta"].(map[string]any)
	if delta != nil {
		w.ensureStarted()
		if s, ok := delta["reasoning_content"].(string); ok && s != "" {
			w.openThinking()
			w.emitDelta(w.x.thinking, map[string]any{"type": "thinking_delta", "thinking": s})
			w.x.thinking.buf.WriteString(s)
		}
		if s, ok := delta["content"].(string); ok && s != "" {
			w.closeThinking()
			w.openText()
			w.emitDelta(w.x.text, map[string]any{"type": "text_delta", "text": s})
			w.x.text.buf.WriteString(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			w.closeThinking()
			w.closeText()
			w.handleToolDeltas(tcs)
		}
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		w.x.stop = anthropicStopReason(fr)
	}
}

func (w *anthropicWriter) handleToolDeltas(tcs []any) {
	for _, ti := range tcs {
		tc, ok := ti.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		blk := w.x.tools[idx]
		if blk == nil {
			blk = &anthBlock{typ: "tool_use"}
			w.x.tools[idx] = blk
			w.x.toolOrder = append(w.x.toolOrder, idx)
		}
		fn, _ := tc["function"].(map[string]any)
		if id, ok := tc["id"].(string); ok && id != "" {
			blk.id = id
		}
		if n, ok := fn["name"].(string); ok && n != "" && blk.name == "" {
			blk.name = n
		}
		if !blk.opened && blk.name != "" {
			blk.idx = w.nextIdx()
			if blk.id == "" {
				blk.id = "toolu_" + strconv.Itoa(blk.idx)
			}
			blk.opened = true
			w.emit("content_block_start", map[string]any{
				"index": blk.idx,
				"content_block": map[string]any{
					"type": "tool_use", "id": blk.id, "name": blk.name, "input": map[string]any{},
				},
			})
		}
		if args, ok := fn["arguments"].(string); ok && args != "" && blk.opened {
			w.emitDelta(blk, map[string]any{"type": "input_json_delta", "partial_json": args})
			blk.buf.WriteString(args)
		}
	}
}

func (w *anthropicWriter) nextIdx() int {
	i := w.x.nextIdx
	w.x.nextIdx++
	return i
}

// ensureStarted 发 message_start（首个增量前一次）。
func (w *anthropicWriter) ensureStarted() {
	if w.x.started {
		return
	}
	w.x.started = true
	w.emit("message_start", map[string]any{
		"message": map[string]any{
			"id": w.x.id, "type": "message", "role": "assistant", "model": w.x.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (w *anthropicWriter) openThinking() {
	if w.x.thinking != nil {
		return
	}
	w.ensureStarted()
	blk := &anthBlock{typ: "thinking", idx: w.nextIdx(), opened: true}
	w.x.thinking = blk
	w.emit("content_block_start", map[string]any{
		"index":         blk.idx,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
}

func (w *anthropicWriter) closeThinking() {
	blk := w.x.thinking
	if blk == nil || blk.closed {
		return
	}
	w.emit("content_block_stop", map[string]any{"index": blk.idx})
	w.x.content = append(w.x.content, map[string]any{"type": "thinking", "thinking": blk.buf.String()})
	blk.closed = true
	w.x.thinking = nil
}

func (w *anthropicWriter) openText() {
	if w.x.text != nil {
		return
	}
	w.ensureStarted()
	blk := &anthBlock{typ: "text", idx: w.nextIdx(), opened: true}
	w.x.text = blk
	w.emit("content_block_start", map[string]any{
		"index":         blk.idx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (w *anthropicWriter) closeText() {
	blk := w.x.text
	if blk == nil || blk.closed {
		return
	}
	w.emit("content_block_stop", map[string]any{"index": blk.idx})
	w.x.content = append(w.x.content, map[string]any{"type": "text", "text": blk.buf.String()})
	blk.closed = true
	w.x.text = nil
}

func (w *anthropicWriter) closeTools() {
	for _, idx := range w.x.toolOrder {
		blk := w.x.tools[idx]
		if blk == nil || blk.closed || !blk.opened {
			continue
		}
		w.emit("content_block_stop", map[string]any{"index": blk.idx})
		var input any = map[string]any{}
		if s := blk.buf.String(); s != "" {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				input = parsed
			}
		}
		w.x.content = append(w.x.content, map[string]any{
			"type": "tool_use", "id": blk.id, "name": blk.name, "input": input,
		})
		blk.closed = true
	}
}

// emitDelta 发 content_block_delta（text/thinking/input_json 三种 delta 共用一个出口）。
func (w *anthropicWriter) emitDelta(blk *anthBlock, delta map[string]any) {
	if blk == nil || blk.closed {
		return
	}
	w.emit("content_block_delta", map[string]any{"index": blk.idx, "delta": delta})
}

// emitCompleted 收尾：content_block_stop → message_delta（stop_reason/usage）→ message_stop。
func (w *anthropicWriter) emitCompleted() {
	if w.x.done || w.x.failed {
		return
	}
	w.closeThinking()
	w.closeText()
	w.closeTools()
	w.ensureStarted()
	stop := w.x.stop
	if stop == "" {
		stop = "end_turn"
	}
	out := int64(0)
	in := int64(0)
	usage := map[string]any{}
	if u := w.x.usage; u != nil {
		if v, ok := u["completion_tokens"].(float64); ok {
			out = int64(v)
		}
		if v, ok := u["prompt_tokens"].(float64); ok {
			in = int64(v)
		}
		if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
			if c, ok := d["cached_tokens"].(float64); ok && c > 0 {
				usage["cache_read_input_tokens"] = int64(c)
			}
		}
	}
	// message_start 的 input_tokens 只能是 0（上游 usage 在末帧才到），这里补上真实值：
	// 比 Anthropic 原生多一个字段，但客户端按需读取，不会因为多字段出错。
	usage["output_tokens"] = out
	if in > 0 {
		usage["input_tokens"] = in
	}
	w.emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": usage,
	})
	w.emit("message_stop", map[string]any{})
	w.x.done = true
}

// emitError 上游 error 帧 → Anthropic error 事件（客户端据此停止并报错）。
func (w *anthropicWriter) emitError(errObj any) {
	if w.x.done || w.x.failed {
		return
	}
	w.x.failed = true
	msg := stringifyArgs(errObj)
	w.emit("error", map[string]any{
		"error": map[string]any{"type": anthropicErrorType(http.StatusBadGateway), "message": msg},
	})
}

// emit 写一个 Anthropic SSE 事件（event: <type>\ndata: {...}）。
func (w *anthropicWriter) emit(typ string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h := w.Header()
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
	}
	_, _ = io.WriteString(w.inner, "event: "+typ+"\ndata: "+string(raw)+"\n\n")
	w.Flush()
}

// finish 收尾：流式补终态事件；非流式把 chat.completion 翻成 Anthropic message。
func (w *anthropicWriter) finish() {
	if w.stream {
		if !w.x.done && !w.x.failed {
			if w.x.started {
				w.emitCompleted()
			} else {
				// 首帧前失败（chat 管道写出的是 JSON 错误）：按原状态码转成 Anthropic
				// 错误信封。伪装成 200 + error 事件会让 Claude Code 的退避/换号逻辑失效。
				w.flushErrorPassthrough()
			}
		}
		return
	}
	w.flushJSON()
}

// flushErrorPassthrough 流式请求在首帧前失败：保留状态码，body 转 Anthropic 错误信封。
func (w *anthropicWriter) flushErrorPassthrough() {
	status := w.status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	body := bytes.TrimSpace(w.body.Bytes())
	msg := ""
	var obj map[string]any
	if len(body) > 0 && json.Unmarshal(body, &obj) == nil {
		if e, ok := obj["error"].(map[string]any); ok {
			msg, _ = e["message"].(string)
			if hint, ok := e["gateway_hint"].(string); ok && hint != "" {
				msg += " (" + hint + ")"
			}
		}
	}
	if msg == "" {
		msg = "upstream returned no data"
	}
	w.Header().Set("Content-Type", "application/json")
	w.inner.WriteHeader(status)
	raw, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicErrorType(status), "message": msg},
	})
	_, _ = w.inner.Write(raw)
}

// flushJSON 非流式：chat.completion → Anthropic message；错误信封 → Anthropic error。
func (w *anthropicWriter) flushJSON() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	body := bytes.TrimSpace(w.body.Bytes())
	if len(body) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.inner.WriteHeader(status)
		return
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		w.Header().Set("Content-Type", "application/json")
		w.inner.WriteHeader(status)
		_, _ = w.inner.Write(body)
		return
	}
	if e, hasErr := obj["error"]; hasErr || status >= 400 {
		msg := ""
		if hasErr {
			if em, ok := e.(map[string]any); ok {
				msg, _ = em["message"].(string)
				if hint, ok := em["gateway_hint"].(string); ok && hint != "" {
					msg += " (" + hint + ")"
				}
			}
		}
		if msg == "" {
			msg = stringifyArgs(obj)
		}
		w.Header().Set("Content-Type", "application/json")
		w.inner.WriteHeader(status)
		raw, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anthropicErrorType(status), "message": msg},
		})
		_, _ = w.inner.Write(raw)
		return
	}
	msg := chatCompletionToAnthropicMessage(obj, w.x)
	w.Header().Set("Content-Type", "application/json")
	w.inner.WriteHeader(status)
	raw, err := json.Marshal(msg)
	if err != nil {
		_, _ = w.inner.Write(body)
		return
	}
	_, _ = w.inner.Write(raw)
}

// chatCompletionToAnthropicMessage chat.completion → Anthropic message 对象。
func chatCompletionToAnthropicMessage(obj map[string]any, x *anthState) map[string]any {
	content := make([]any, 0, 3)
	stop := "end_turn"
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			if fr, _ := c["finish_reason"].(string); fr != "" {
				stop = anthropicStopReason(fr)
			}
			if msg, ok := c["message"].(map[string]any); ok {
				if s, ok := msg["reasoning_content"].(string); ok && strings.TrimSpace(s) != "" {
					content = append(content, map[string]any{"type": "thinking", "thinking": s})
				}
				if s, ok := msg["content"].(string); ok && s != "" {
					content = append(content, map[string]any{"type": "text", "text": s})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, ti := range tcs {
						tc, ok := ti.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tc["function"].(map[string]any)
						name, _ := fn["name"].(string)
						var input any = map[string]any{}
						if s := stringifyArgs(fn["arguments"]); s != "" {
							var parsed any
							if json.Unmarshal([]byte(s), &parsed) == nil {
								input = parsed
							}
						}
						id, _ := tc["id"].(string)
						content = append(content, map[string]any{
							"type": "tool_use", "id": id, "name": name, "input": input,
						})
					}
				}
			}
		}
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if u, ok := obj["usage"].(map[string]any); ok {
		if v, ok := u["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := u["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
	}
	return map[string]any{
		"id":            x.id,
		"type":          "message",
		"role":          "assistant",
		"model":         x.model,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage":         usage,
	}
}

// anthropicStopReason chat finish_reason → Anthropic stop_reason。
// 未知值按 end_turn（上游还会出 content_filter 等，Anthropic 无对应语义）。
func anthropicStopReason(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop":
		return "end_turn"
	}
	return "end_turn"
}

// anthropicErrorType HTTP 状态 → Anthropic error.type。
func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusRequestEntityTooLarge:
		return "invalid_request_error"
	}
	return "api_error"
}

// writeAnthropicError 直接写一个 Anthropic error 信封（协议层错误用）。
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	raw, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
	_, _ = w.Write(raw)
}
