// responses.go OpenAI Responses 入站协议（POST /v1/responses）：把 Responses 请求
// 翻成 chat.completions 请求交给既有 chatCompletions 处理，再把它写出的 SSE/JSON
// 翻回 Responses 事件流。
//
// 为什么用「转成 chat + 拦截出站」而不是另写一条上游管道：选号、轮转、错误分类、
// 冷却/熔断、提示词改写、指纹脱敏、DSML 标记还原、零输出守卫……全在 chatCompletions
// 里，那条路径每加一个修复本适配器都自动获得。适配器只做**协议翻译**，不碰调度。
//
// 覆盖范围（对齐 Codex 实际用法）：
//   - input：字符串或 items 数组（message / function_call / function_call_output / reasoning 占位）
//   - instructions → system 消息；reasoning.effort → reasoning_effort
//   - tools：function 与 custom（custom 折叠成单参 function，见 responsesToolToChat）
//   - 流式：response.created / output_item.* / output_text.* / reasoning_summary_* /
//     function_call_arguments.* / completed | incomplete | failed，每事件带 sequence_number
//   - 非流式：chat.completion → response 对象（output 数组含 reasoning / message / function_call）
//
// 不做的（明确说明而非静默错）：previous_response_id / store 之类的**服务端会话状态**
// 一律不支持（Codex 每轮带全量 input，用不到）；GET /v1/responses 的 WebSocket 升级
// 直接回 426（客户端据此立刻回落 HTTPS，不会空转重试）。
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

// responsesWebsocketUnsupported 回应 Codex 的 WS 升级探测（GET /v1/responses）。
//
// 426 是客户端**立刻且粘性**回落 HTTPS 的信号（Codex 的 websocket_fallback 用例：
// 见到 426 只握手一次就转 POST，否则要空转 max_retries 次，日志里表现为一串
// 「Reconnecting… n/5」）。本网关不保存 previous_response_id/连接内状态，WS 复用无从谈起。
func (h *Handler) responsesWebsocketUnsupported(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Upgrade", "websocket")
	http.Error(w, "websocket transport unsupported; use POST /v1/responses (set supports_websockets = false)",
		http.StatusUpgradeRequired)
}

// responses 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	// 与 chat 主通道同口径：请求体不做网关侧上限（README/上游对齐口径），
	// 超限类问题交由上游自然响应；此处不能比主通道更严，否则大上下文/多图请求
	// 会在适配层被拒，表现为「同一个请求换个协议就失败」。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chat, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(chat, &peek)

	// 复用 chat 管道：克隆请求（context 里带着 requestTrace，流水行/归档照常工作）。
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chat))
	r2.ContentLength = int64(len(chat))
	rw := newResponsesWriter(w, peek.Stream, modelBare(peek.Model))
	defer rw.finish()
	h.chatCompletions(rw, r2)
}

// modelBare 去掉网关路由前缀（cn:/global:），供 response 对象回显裸 model 名。
func modelBare(model string) string {
	if _, bare := resolveModel(model); bare != "" {
		return bare
	}
	return model
}

// responsesToChat 把 Responses 请求体翻译成 chat.completions 请求体。
//
// 只翻译「语义等价」的字段；无法等价的一律**显式忽略**（不猜）：
//   - input 的 reasoning item：上游不需要回放思考内容（思考由上游重新生成）；
//   - include / store / previous_response_id / truncation：服务端会话能力，不支持。
func responsesToChat(src []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	out := map[string]any{}
	// 直接透传的字段：语义与 chat.completions 完全一致。
	// 只透传 chat.completions 也有的字段：Responses 的 metadata 在 chat 协议里
	// 没有定义，透传只会让上游多收一个未知键（部分版本会直接 400 参数错误）。
	for _, k := range []string{
		"model", "stream", "stream_options", "temperature", "top_p", "user", "n",
		"stop", "parallel_tool_calls", "prompt_cache_key",
	} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	// max_output_tokens 是 Responses 的叫法，上游认 max_tokens。
	if v, ok := obj["max_output_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	} else if v, ok := obj["max_completion_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	}
	// reasoning.effort → reasoning_effort（上游按档位注思考预算）。
	if r, ok := obj["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"]; ok && e != nil {
			out["reasoning_effort"] = e
		}
	}
	if v, ok := obj["reasoning_effort"]; ok && v != nil {
		out["reasoning_effort"] = v
	}

	msgs := make([]any, 0, 8)
	if inst, ok := obj["instructions"].(string); ok && strings.TrimSpace(inst) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": inst})
	}
	switch in := obj["input"].(type) {
	case string:
		if strings.TrimSpace(in) != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": in})
		}
	case []any:
		msgs = append(msgs, responsesInputToMessages(in)...)
	case nil:
		// 无 input：交给上游报参数错误（不伪造空对话）。
	default:
		return nil, fmt.Errorf("input must be a string or an array of items")
	}
	out["messages"] = msgs

	if tools, ok := obj["tools"].([]any); ok {
		if chatTools := responsesToolsToChat(tools); len(chatTools) > 0 {
			out["tools"] = chatTools
		}
	}
	if tc, ok := obj["tool_choice"]; ok && tc != nil {
		out["tool_choice"] = responsesToolChoiceToChat(tc)
	}
	// 结构化输出：Responses 的 text.format 与 chat 的 response_format 同构（json_schema/json_object）。
	if t, ok := obj["text"].(map[string]any); ok {
		if f, ok := t["format"].(map[string]any); ok {
			if rf := responsesTextFormatToChat(f); rf != nil {
				out["response_format"] = rf
			}
		}
	}
	return json.Marshal(out)
}

// responsesInputToMessages 把 Responses 的 input items 翻成 chat messages。
// 逐类处理，未知类型跳过（记一行日志比伪造消息更安全——伪造会让模型看到不存在的轮次）。
func responsesInputToMessages(items []any) []any {
	msgs := make([]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		role, _ := m["role"].(string)
		switch typ {
		case "", "message":
			// 也接受纯 chat 形态（部分客户端直接塞 {role, content}）。
			mm := responsesMessageToChat(m, role)
			if mm != nil {
				msgs = append(msgs, mm)
			}
		case "function_call":
			// 历史轮回放：assistant 发起的工具调用。同一轮多个调用合并到一条 assistant 消息，
			// 否则上游会看到「assistant 消息之间夹着空轮」，破坏工具配对。
			callID, _ := m["call_id"].(string)
			name, _ := m["name"].(string)
			args := stringifyArgs(m["arguments"])
			tc := map[string]any{
				"id":       callID,
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			}
			if n := len(msgs); n > 0 {
				if last, ok := msgs[n-1].(map[string]any); ok {
					if _, isAsst := last["tool_calls"]; isAsst {
						existing, _ := last["tool_calls"].([]any)
						last["tool_calls"] = append(existing, tc)
						continue
					}
				}
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{tc}})
		case "function_call_output":
			callID, _ := m["call_id"].(string)
			out := stringifyArgs(m["output"])
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": callID, "content": out})
		case "reasoning", "item_reference":
			// reasoning item 不回放（思考由上游重新生成）；item_reference 依赖服务端状态，不支持。
			continue
		default:
			continue
		}
	}
	return msgs
}

// responsesMessageToChat 单条 message item → chat message（内容块数组归一为字符串或
// OpenAI 多模态 parts）。role 缺失按 user 处理（Responses 的 message item 允许省略）。
func responsesMessageToChat(m map[string]any, role string) map[string]any {
	if role == "" {
		role = "user"
	}
	switch c := m["content"].(type) {
	case string:
		return map[string]any{"role": role, "content": c}
	case []any:
		var text strings.Builder
		parts := make([]any, 0, len(c))
		hasImage := false
		for _, pi := range c {
			p, ok := pi.(map[string]any)
			if !ok {
				continue
			}
			switch p["type"] {
			case "input_text", "output_text", "text", "summary_text":
				s, _ := p["text"].(string)
				text.WriteString(s)
			case "input_image":
				url, _ := p["image_url"].(string)
				if url == "" {
					// Responses 也允许直接把 base64 放 image_url（对象/字符串两种形态）。
					if u, ok := p["image_url"].(map[string]any); ok {
						url, _ = u["url"].(string)
					}
				}
				if url != "" {
					hasImage = true
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": url},
					})
				}
			}
		}
		if !hasImage {
			return map[string]any{"role": role, "content": text.String()}
		}
		// 有图：文本作为 text part 保留（顺序信息在上游侧不敏感）。
		if text.Len() > 0 {
			parts = append([]any{map[string]any{"type": "text", "text": text.String()}}, parts...)
		}
		return map[string]any{"role": role, "content": parts}
	default:
		return map[string]any{"role": role, "content": ""}
	}
}

// responsesToolsToChat 工具声明翻译：
//   - {"type":"function","name","description","parameters","strict"} → chat 的嵌套形
//   - {"type":"custom","name","description","format"} → 单参 function（参数名 input）
//     Codex 的 custom 工具（如 apply_patch）语义是「传一段自由文本」，折叠成单参
//     function 后模型仍能正确调用；回程按 function_call 下发，客户端按名字执行。
//   - 托管工具（{"type":"web_search"} 等，无 name）本网关暂不执行，直接丢弃——
//     留着会让模型以为能联网却拿不到结果。
func responsesToolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, ti := range tools {
		t, ok := ti.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := t["type"].(string)
		switch typ {
		case "function":
			name, _ := t["name"].(string)
			if name == "" {
				continue
			}
			fn := map[string]any{"name": name}
			if d, ok := t["description"].(string); ok && d != "" {
				fn["description"] = d
			}
			if p, ok := t["parameters"]; ok && p != nil {
				fn["parameters"] = p
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		case "custom":
			name, _ := t["name"].(string)
			if name == "" {
				continue
			}
			fn := map[string]any{
				"name": name,
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"input": map[string]any{"type": "string", "description": "raw tool input"}},
					"required":   []any{"input"},
				},
			}
			if d, ok := t["description"].(string); ok && d != "" {
				fn["description"] = d
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		default:
			// 托管/内置工具（web_search / file_search / computer_use…）：网关不执行，丢弃。
			continue
		}
	}
	return out
}

// responsesToolChoiceToChat tool_choice 翻译：
// "auto"/"none"/"required" 直通；{"type":"function","name":X} → 嵌套形；
// {"type":"custom","name":X} 同样是嵌套形（对应被折叠成 function 的 custom 工具）。
func responsesToolChoiceToChat(tc any) any {
	m, ok := tc.(map[string]any)
	if !ok {
		return tc
	}
	name, _ := m["name"].(string)
	if name == "" {
		return "auto"
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}
}

// responsesTextFormatToChat text.format → chat 的 response_format。
// json_schema 的字段名一致（name/schema/strict），只需补 type 映射。
func responsesTextFormatToChat(f map[string]any) map[string]any {
	switch f["type"] {
	case "json_schema":
		out := map[string]any{"type": "json_schema"}
		for _, k := range []string{"name", "schema", "strict", "description"} {
			if v, ok := f[k]; ok && v != nil {
				out[k] = v
			}
		}
		return map[string]any{"type": "json_schema", "json_schema": out}
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "text":
		return nil // 纯文本是缺省，无需下发
	}
	return nil
}

// stringifyArgs 把 JSON 值渲染成字符串（工具参数/工具输出都可能是对象）。
func stringifyArgs(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// ---------------------------------------------------------------------------
// 出站：chat 流/JSON → Responses 事件流
// ---------------------------------------------------------------------------

// respItem 一个正在生成（或已收尾）的 Responses output item。
type respItem struct {
	idx       int
	id        string
	typ       string // reasoning | message | function_call
	status    string // in_progress | completed
	callID    string
	name      string
	text      strings.Builder // reasoning summary / message 正文
	args      strings.Builder // function_call 参数
	added     bool            // output_item.added 已发
	partAdded bool            // content_part / summary_part 已发
	closed    bool            // output_item.done 已发
}

// respState 单次 Responses 请求的出站状态机。
type respState struct {
	id        string
	model     string
	createdAt int64
	seq       int
	started   bool // response.created / in_progress 已发
	output    []map[string]any
	nextIdx   int

	reasoning *respItem
	message   *respItem
	tools     map[int]*respItem // 上游 tool_call index → item
	toolOrder []int

	finish       string
	usage        map[string]any
	completed    bool
	failed       bool
	status       string // completed | incomplete | failed
	incompleteDt map[string]any
}

// responsesWriter 拦截 chatCompletions 的写出并翻译成 Responses 协议。
type responsesWriter struct {
	inner  http.ResponseWriter
	stream bool
	model  string
	status int
	body   bytes.Buffer // 非流式：聚合 JSON；流式：未成帧的残余字节
	x      *respState
}

func newResponsesWriter(w http.ResponseWriter, stream bool, model string) *responsesWriter {
	return &responsesWriter{
		inner:  w,
		stream: stream,
		model:  model,
		x: &respState{
			id:        "resp_" + strconv.FormatInt(time.Now().UnixNano(), 36),
			model:     model,
			createdAt: time.Now().Unix(),
			tools:     map[int]*respItem{},
		},
	}
}

func (w *responsesWriter) Header() http.Header { return w.inner.Header() }

// WriteHeader 只记录状态码：真正提交由翻译层决定（响应头可能要换成 SSE）。
func (w *responsesWriter) WriteHeader(code int) { w.status = code }

func (w *responsesWriter) Flush() {
	if f, ok := w.inner.(http.Flusher); ok {
		f.Flush()
	}
}

// Write 收下 chatCompletions 的输出：流式按 SSE 帧解析翻译，非流式先攒整包。
func (w *responsesWriter) Write(p []byte) (int, error) {
	if !w.stream {
		w.body.Write(p)
		return len(p), nil
	}
	w.body.Write(p)
	w.drainFrames()
	return len(p), nil
}

// drainFrames 从缓冲区里切出完整 SSE 帧并逐帧翻译。
func (w *responsesWriter) drainFrames() {
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
				continue // 注释/event 行：无需翻译（本适配器自产事件）
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				return
			}
			w.handleChunk(payload)
		}
	}
}

// handleChunk 翻译单个 chat chunk。
func (w *responsesWriter) handleChunk(payload string) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return
	}
	// 上游错误帧（chat 管道透传的 error）：转成 response.failed，客户端据此重试/报错。
	if e, ok := obj["error"]; ok {
		w.emitFailed(e)
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
			w.openReasoning()
			w.emit("response.reasoning_summary_text.delta", map[string]any{
				"item_id":       w.x.reasoning.id,
				"output_index":  w.x.reasoning.idx,
				"summary_index": 0,
				"delta":         s,
			})
			w.x.reasoning.text.WriteString(s)
		}
		if s, ok := delta["content"].(string); ok && s != "" {
			w.closeReasoning()
			w.openMessage()
			w.emit("response.output_text.delta", map[string]any{
				"item_id":       w.x.message.id,
				"output_index":  w.x.message.idx,
				"content_index": 0,
				"delta":         s,
			})
			w.x.message.text.WriteString(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			w.closeReasoning()
			w.closeMessage()
			w.handleToolDeltas(tcs)
		}
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		w.x.finish = fr
	}
}

// handleToolDeltas 处理工具调用分片：首片建 item（added），后续只发参数增量。
func (w *responsesWriter) handleToolDeltas(tcs []any) {
	for _, ti := range tcs {
		tc, ok := ti.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		it := w.x.tools[idx]
		if it == nil {
			it = &respItem{typ: "function_call", status: "in_progress"}
			w.x.tools[idx] = it
			w.x.toolOrder = append(w.x.toolOrder, idx)
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if id, ok := tc["id"].(string); ok && id != "" {
				it.callID = id
			}
			if n, ok := fn["name"].(string); ok && n != "" && it.name == "" {
				it.name = n
			}
		}
		if !it.added && it.name != "" {
			it.idx = w.nextIdx()
			it.id = it.callID
			if it.id == "" {
				it.id = "call_" + strconv.Itoa(it.idx)
				it.callID = it.id
			}
			it.added = true
			w.emit("response.output_item.added", map[string]any{
				"output_index": it.idx,
				"item": map[string]any{
					"id":        it.id,
					"type":      "function_call",
					"status":    "in_progress",
					"call_id":   it.callID,
					"name":      it.name,
					"arguments": "",
				},
			})
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if s, ok := fn["arguments"].(string); ok && s != "" && it.added {
				w.emit("response.function_call_arguments.delta", map[string]any{
					"item_id":      it.id,
					"output_index": it.idx,
					"delta":        s,
				})
				it.args.WriteString(s)
			}
		}
	}
}

func (w *responsesWriter) nextIdx() int {
	i := w.x.nextIdx
	w.x.nextIdx++
	return i
}

// ensureStarted 发 response.created + response.in_progress（首个增量帧前一次）。
func (w *responsesWriter) ensureStarted() {
	if w.x.started {
		return
	}
	w.x.started = true
	resp := w.responseObject("in_progress")
	w.emit("response.created", map[string]any{"response": resp})
	w.emit("response.in_progress", map[string]any{"response": resp})
}

func (w *responsesWriter) openReasoning() {
	if w.x.reasoning != nil || w.reasoningDone() {
		return
	}
	w.ensureStarted()
	it := &respItem{typ: "reasoning", status: "in_progress", id: "rs_" + w.x.id, idx: w.nextIdx(), added: true}
	w.x.reasoning = it
	w.emit("response.output_item.added", map[string]any{
		"output_index": it.idx,
		"item":         map[string]any{"id": it.id, "type": "reasoning", "summary": []any{}},
	})
	w.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id":       it.id,
		"output_index":  it.idx,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text"},
	})
	it.partAdded = true
}

// reasoningDone 标记已收尾的 reasoning（同一响应只出一个 reasoning item）。
func (w *responsesWriter) reasoningDone() bool {
	for _, o := range w.x.output {
		if o["type"] == "reasoning" {
			return true
		}
	}
	return false
}

// closeReasoning 收尾 reasoning item（summary_text.done / part.done / item.done）。
func (w *responsesWriter) closeReasoning() {
	it := w.x.reasoning
	if it == nil || it.closed {
		return
	}
	text := it.text.String()
	if text != "" {
		w.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id":       it.id,
			"output_index":  it.idx,
			"summary_index": 0,
			"text":          text,
		})
	}
	w.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id":       it.id,
		"output_index":  it.idx,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": text},
	})
	item := map[string]any{
		"id":      it.id,
		"type":    "reasoning",
		"status":  "completed",
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
	w.emit("response.output_item.done", map[string]any{"output_index": it.idx, "item": item})
	w.x.output = append(w.x.output, item)
	it.closed = true
	w.x.reasoning = nil
}

// openMessage 打开 assistant message item（content_part.added 一次）。
func (w *responsesWriter) openMessage() {
	if w.x.message != nil {
		return
	}
	w.ensureStarted()
	it := &respItem{typ: "message", status: "in_progress", id: "msg_" + w.x.id, idx: w.nextIdx(), added: true}
	w.x.message = it
	w.emit("response.output_item.added", map[string]any{
		"output_index": it.idx,
		"item": map[string]any{
			"id": it.id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{},
		},
	})
	w.emit("response.content_part.added", map[string]any{
		"item_id":       it.id,
		"output_index":  it.idx,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
	it.partAdded = true
}

// closeMessage 收尾 message item。
func (w *responsesWriter) closeMessage() {
	it := w.x.message
	if it == nil || it.closed {
		return
	}
	text := it.text.String()
	if text != "" {
		w.emit("response.output_text.done", map[string]any{
			"item_id":       it.id,
			"output_index":  it.idx,
			"content_index": 0,
			"text":          text,
		})
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		w.emit("response.content_part.done", map[string]any{
			"item_id":       it.id,
			"output_index":  it.idx,
			"content_index": 0,
			"part":          part,
		})
		item := map[string]any{
			"id": it.id, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{part},
		}
		w.emit("response.output_item.done", map[string]any{"output_index": it.idx, "item": item})
		w.x.output = append(w.x.output, item)
	} else {
		// 空正文：不留空 message（客户端会渲染出一个空气泡）。
		item := map[string]any{"id": it.id, "type": "message", "role": "assistant", "status": "completed", "content": []any{}}
		w.emit("response.output_item.done", map[string]any{"output_index": it.idx, "item": item})
		w.x.output = append(w.x.output, item)
	}
	it.closed = true
	w.x.message = nil
}

// closeTools 收尾所有工具调用 item（参数累积完成）。
func (w *responsesWriter) closeTools() {
	for _, idx := range w.x.toolOrder {
		it := w.x.tools[idx]
		if it == nil || it.closed || !it.added {
			continue
		}
		args := it.args.String()
		w.emit("response.function_call_arguments.done", map[string]any{
			"item_id":      it.id,
			"output_index": it.idx,
			"arguments":    args,
		})
		item := map[string]any{
			"id": it.id, "type": "function_call", "status": "completed",
			"call_id": it.callID, "name": it.name, "arguments": args,
		}
		w.emit("response.output_item.done", map[string]any{"output_index": it.idx, "item": item})
		w.x.output = append(w.x.output, item)
		it.closed = true
	}
}

// emitCompleted 发终态事件（completed / incomplete）。工具调用缺失的收尾项在此补 done。
func (w *responsesWriter) emitCompleted() {
	if w.x.completed || w.x.failed {
		return
	}
	w.closeReasoning()
	w.closeMessage()
	w.closeTools()
	w.ensureStarted()
	typ, status := "response.completed", "completed"
	if w.x.finish == "length" || w.x.incompleteDt != nil {
		typ, status = "response.incomplete", "incomplete"
		if w.x.incompleteDt == nil {
			w.x.incompleteDt = map[string]any{"reason": "max_output_tokens"}
		}
	}
	w.x.completed = true
	w.x.status = status
	w.emit(typ, map[string]any{"response": w.responseObject(status)})
}

// emitFailed 上游错误帧 → response.failed（含 error 对象），只发一次。
func (w *responsesWriter) emitFailed(errObj any) {
	if w.x.failed || w.x.completed {
		return
	}
	w.x.failed = true
	w.x.status = "failed"
	w.ensureStarted()
	obj := w.responseObject("failed")
	obj["error"] = errObj
	w.emit("response.failed", map[string]any{"response": obj})
}

// responseObject 组装 response 对象（created / in_progress / completed / failed 共用）。
func (w *responsesWriter) responseObject(status string) map[string]any {
	obj := map[string]any{
		"id":         w.x.id,
		"object":     "response",
		"created_at": w.x.createdAt,
		"status":     status,
		"model":      w.x.model,
		"output":     w.x.output,
	}
	if len(w.x.output) == 0 {
		obj["output"] = []any{}
	}
	if w.x.incompleteDt != nil {
		obj["incomplete_details"] = w.x.incompleteDt
	}
	if u := responsesUsage(w.x.usage); u != nil {
		obj["usage"] = u
	}
	return obj
}

// emit 写一个 Responses SSE 事件（event: + data:，含递增 sequence_number）。
func (w *responsesWriter) emit(typ string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	payload["sequence_number"] = w.x.seq
	w.x.seq++
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

// finish 收尾：流式补终态事件；非流式把聚合 JSON 翻成 response 对象写出。
func (w *responsesWriter) finish() {
	if w.stream {
		// 正常结束（含客户端提前断开）：保证有且只有一个终态事件。
		if !w.x.completed && !w.x.failed {
			if w.x.started {
				w.emitCompleted()
			} else {
				// 一帧 SSE 都没开始：chat 管道大概率在开流前就失败了，写出的是 JSON
				// 错误信封。按原状态码/原文透出——把 400/429/503 降级成「200 +
				// response.failed」会让客户端误判可重试性（Codex 依赖 HTTP 码决定
				// 重试/换模型），也丢掉了 gateway_hint。
				w.flushErrorPassthrough()
			}
		}
		return
	}
	w.flushJSON()
}

// flushErrorPassthrough 流式请求在首帧前失败：把 JSON 错误按原状态码透出，
// 而不是伪装成 200 的 SSE 事件。body 不是 JSON 时包一层标准错误信封。
func (w *responsesWriter) flushErrorPassthrough() {
	status := w.status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	body := bytes.TrimSpace(w.body.Bytes())
	w.Header().Set("Content-Type", "application/json")
	var obj map[string]any
	if len(body) > 0 && json.Unmarshal(body, &obj) == nil {
		w.inner.WriteHeader(status)
		_, _ = w.inner.Write(body)
		return
	}
	w.inner.WriteHeader(status)
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": "upstream returned no data", "type": "upstream_error", "code": "upstream_parse",
	}})
	_, _ = w.inner.Write(raw)
}

// flushJSON 非流式：chat.completion（或错误信封）→ response 对象。
func (w *responsesWriter) flushJSON() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	body := bytes.TrimSpace(w.body.Bytes())
	if len(body) == 0 {
		w.inner.WriteHeader(status)
		return
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		w.inner.WriteHeader(status)
		_, _ = w.inner.Write(body)
		return
	}
	// 错误信封原样透出（chat 管道的错误文案/hint 已在里面，翻译反而丢信息）。
	if _, hasErr := obj["error"]; hasErr || status >= 400 {
		w.inner.WriteHeader(status)
		_, _ = w.inner.Write(body)
		return
	}
	w.inner.WriteHeader(status)
	raw, err := json.Marshal(chatCompletionToResponse(obj, w.x))
	if err != nil {
		_, _ = w.inner.Write(body)
		return
	}
	_, _ = w.inner.Write(raw)
}

// chatCompletionToResponse 非流式 chat.completion → Responses response 对象。
func chatCompletionToResponse(obj map[string]any, x *respState) map[string]any {
	output := make([]any, 0, 3)
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			if msg, ok := c["message"].(map[string]any); ok {
				if s, ok := msg["reasoning_content"].(string); ok && strings.TrimSpace(s) != "" {
					output = append(output, map[string]any{
						"id": "rs_" + x.id, "type": "reasoning",
						"summary": []any{map[string]any{"type": "summary_text", "text": s}},
					})
				}
				if s, ok := msg["content"].(string); ok && s != "" {
					output = append(output, map[string]any{
						"id": "msg_" + x.id, "type": "message", "role": "assistant", "status": "completed",
						"content": []any{map[string]any{"type": "output_text", "text": s, "annotations": []any{}}},
					})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for i, ti := range tcs {
						tc, ok := ti.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tc["function"].(map[string]any)
						name, _ := fn["name"].(string)
						args := stringifyArgs(fn["arguments"])
						callID, _ := tc["id"].(string)
						if callID == "" {
							callID = "call_" + strconv.Itoa(i)
						}
						output = append(output, map[string]any{
							"id": callID, "type": "function_call", "status": "completed",
							"call_id": callID, "name": name, "arguments": args,
						})
					}
				}
			}
		}
	}
	resp := map[string]any{
		"id":         x.id,
		"object":     "response",
		"created_at": x.createdAt,
		"status":     "completed",
		"model":      x.model,
		"output":     output,
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		// 参数与流式一致：只翻译 token 口径，不额外编造（缓存读数是 Codex 关心的那项）。
		if cu := responsesUsage(u); cu != nil {
			resp["usage"] = cu
		}
	}
	// finish_reason=length → incomplete（如实告知被截断，客户端据此决定续写）。
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			if fr, _ := c["finish_reason"].(string); fr == "length" {
				resp["status"] = "incomplete"
				resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			}
		}
	}
	return resp
}

// responsesUsage chat usage → Responses usage（含 cached_tokens 明细）。
func responsesUsage(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	out := map[string]any{}
	if v, ok := u["prompt_tokens"]; ok {
		out["input_tokens"] = v
	}
	if v, ok := u["completion_tokens"]; ok {
		out["output_tokens"] = v
	}
	if v, ok := u["total_tokens"]; ok {
		out["total_tokens"] = v
	}
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if c, ok := d["cached_tokens"]; ok && c != nil {
			out["input_tokens_details"] = map[string]any{"cached_tokens": c}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
