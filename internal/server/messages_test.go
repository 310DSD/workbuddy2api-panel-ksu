// messages_test.go /v1/messages（Anthropic）适配：请求翻译、事件流、错误信封、鉴权。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestMessagesToChatTranslation 请求翻译：system（string/blocks）、text/image/tool_use/
// tool_result 块、thinking 丢弃、tools.input_schema、tool_choice.any→required、
// thinking.budget_tokens→reasoning_effort、stop_sequences→stop。
func TestMessagesToChatTranslation(t *testing.T) {
	src := `{
	  "model":"cn:claude-sonnet",
	  "max_tokens":1024,
	  "temperature":0.3,
	  "stop_sequences":["END"],
	  "stream":true,
	  "thinking":{"type":"enabled","budget_tokens":4096},
	  "system":[{"type":"text","text":"你是助手"}],
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"看图"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
	    {"role":"assistant","content":[{"type":"thinking","thinking":"旧思考"},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"/a"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"文件内容"}]}]}
	  ],
	  "tools":[{"name":"read_file","description":"读文件","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
	  "tool_choice":{"type":"any"}
	}`
	raw, meta, err := messagesToChat([]byte(src))
	if err != nil {
		t.Fatalf("messagesToChat: %v", err)
	}
	if !meta.stream || meta.model != "cn:claude-sonnet" {
		t.Fatalf("meta=%+v", meta)
	}
	var chat struct {
		MaxTokens    int              `json:"max_tokens"`
		Temperature  float64          `json:"temperature"`
		Stop         []any            `json:"stop"`
		Reasoning    string           `json:"reasoning_effort"`
		Messages     []map[string]any `json:"messages"`
		Tools        []map[string]any `json:"tools"`
		ToolChoice   string           `json:"tool_choice"`
		HasToolChoic bool             `json:"-"`
	}
	if err := json.Unmarshal(raw, &chat); err != nil {
		t.Fatalf("翻译结果不是合法 JSON：%v (%s)", err, raw)
	}
	if chat.MaxTokens != 1024 || chat.Temperature != 0.3 {
		t.Errorf("max_tokens/temperature=%d/%v", chat.MaxTokens, chat.Temperature)
	}
	if len(chat.Stop) != 1 || chat.Stop[0] != "END" {
		t.Errorf("stop_sequences 应映射到 stop：%v", chat.Stop)
	}
	if chat.Reasoning != "medium" {
		t.Errorf("budget 4096 应折算 medium，got %q", chat.Reasoning)
	}
	if chat.ToolChoice != "required" {
		t.Errorf("tool_choice any 应映射 required，got %q", chat.ToolChoice)
	}
	// messages：system + user(多模态) + assistant(tool_calls) + tool
	if len(chat.Messages) != 4 {
		t.Fatalf("messages=%d want 4: %+v", len(chat.Messages), chat.Messages)
	}
	if chat.Messages[0]["role"] != "system" || chat.Messages[0]["content"] != "你是助手" {
		t.Errorf("system blocks 应拼成 system 消息：%+v", chat.Messages[0])
	}
	parts, _ := chat.Messages[1]["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("多模态 user content=%+v", chat.Messages[1]["content"])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("图片块应转 image_url：%+v", img)
	}
	if u := img["image_url"].(map[string]any)["url"].(string); !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Errorf("base64 源应转 data URI，got %q", u)
	}
	if chat.Messages[2]["role"] != "assistant" {
		t.Errorf("tool_use 应变成 assistant.tool_calls：%+v", chat.Messages[2])
	}
	tcs, _ := chat.Messages[2]["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%v", chat.Messages[2]["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "toolu_1" || tc["function"].(map[string]any)["name"] != "read_file" {
		t.Errorf("tool_call 翻译错：%+v", tc)
	}
	if chat.Messages[3]["role"] != "tool" || chat.Messages[3]["tool_call_id"] != "toolu_1" {
		t.Errorf("tool_result 应变成 tool 消息：%+v", chat.Messages[3])
	}
	if chat.Messages[3]["content"] != "文件内容" {
		t.Errorf("tool_result 文本=%v", chat.Messages[3]["content"])
	}
	// tools：input_schema → function.parameters
	if len(chat.Tools) != 1 {
		t.Fatalf("tools=%+v", chat.Tools)
	}
	fn := chat.Tools[0]["function"].(map[string]any)
	if fn["name"] != "read_file" {
		t.Errorf("工具名=%v", fn["name"])
	}
	if _, ok := fn["parameters"].(map[string]any)["properties"]; !ok {
		t.Errorf("input_schema 未映射到 parameters：%+v", fn["parameters"])
	}
}

// TestMessagesNonStream 非流式：chat.completion → Anthropic message（thinking/text/usage）。
func TestMessagesNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"想一想\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\n" +
			"data: [DONE]\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "k"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "k")
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var msg struct {
		Type       string           `json:"type"`
		Role       string           `json:"role"`
		Model      string           `json:"model"`
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("响应不是 JSON：%v (%s)", err, rec.Body.String())
	}
	if msg.Type != "message" || msg.Role != "assistant" || msg.Model != "glm-5.2" {
		t.Fatalf("message 头=%+v", msg)
	}
	if len(msg.Content) != 2 || msg.Content[0]["type"] != "thinking" || msg.Content[1]["type"] != "text" {
		t.Fatalf("content 块=%+v", msg.Content)
	}
	if msg.Content[1]["text"] != "你好" {
		t.Errorf("正文=%v", msg.Content[1]["text"])
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason=%q want end_turn", msg.StopReason)
	}
	if msg.Usage["input_tokens"].(float64) != 9 || msg.Usage["output_tokens"].(float64) != 4 {
		t.Errorf("usage=%+v", msg.Usage)
	}
}

// TestMessagesNonStreamToolUse 工具调用：content 里出 tool_use 块（input 已解析成对象），
// stop_reason=tool_use（Claude Code 据此执行工具）。
func TestMessagesNonStreamToolUse(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"/a\\\"}\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)))

	var msg struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("响应不是 JSON：%v (%s)", err, rec.Body.String())
	}
	if msg.StopReason != "tool_use" {
		t.Fatalf("stop_reason=%q want tool_use", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0]["type"] != "tool_use" {
		t.Fatalf("content=%+v", msg.Content)
	}
	blk := msg.Content[0]
	if blk["id"] != "call_a" || blk["name"] != "read_file" {
		t.Errorf("tool_use 块=%+v", blk)
	}
	input, _ := blk["input"].(map[string]any)
	if input["path"] != "/a" {
		t.Errorf("input 应解析成对象：%+v", blk["input"])
	}
}

// TestMessagesStreamEvents 流式事件序列：message_start → thinking 块 → text 块 →
// message_delta（stop_reason）→ message_stop。
func TestMessagesStreamEvents(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"思考\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
			"data: [DONE]\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	evs := respEvents(t, rec.Body.String())
	types := eventTypes(evs)
	want := []string{
		"message_start",
		"content_block_start", // thinking
		"content_block_delta",
		"content_block_stop",
		"content_block_start", // text
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列不符：\n got=%v\nwant=%v", types, want)
	}
	// message_delta 带 stop_reason 与 output_tokens。
	md := evs[len(evs)-2]
	delta := md.Data["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", delta["stop_reason"])
	}
	if md.Data["usage"].(map[string]any)["output_tokens"].(float64) != 2 {
		t.Errorf("usage=%v", md.Data["usage"])
	}
	// thinking 块的正文增量正确。
	if evs[2].Data["delta"].(map[string]any)["thinking"] != "思考" {
		t.Errorf("thinking_delta=%v", evs[2].Data)
	}
}

// TestMessagesStreamToolUse 流式工具调用：tool_use 块 + input_json_delta 累积。
func TestMessagesStreamToolUse(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"/a\\\"}\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"read_file","input_schema":{"type":"object"}}]}`)))

	evs := respEvents(t, rec.Body.String())
	var starts, deltas, stops int
	for _, e := range evs {
		switch e.Type {
		case "content_block_start":
			if cb, _ := e.Data["content_block"].(map[string]any); cb["type"] == "tool_use" {
				starts++
				if cb["name"] != "read_file" || cb["id"] != "call_a" {
					t.Errorf("tool_use 块=%+v", cb)
				}
			}
		case "content_block_delta":
			if d, _ := e.Data["delta"].(map[string]any); d["type"] == "input_json_delta" {
				deltas++
			}
		case "content_block_stop":
			stops++
		}
	}
	if starts != 1 || deltas != 2 || stops != 1 {
		t.Fatalf("starts=%d deltas=%d stops=%d", starts, deltas, stops)
	}
	md := evs[len(evs)-2]
	if md.Data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", md.Data["delta"])
	}
}

// TestMessagesAuthXAPIKey x-api-key 鉴权：错 key → 401 + Anthropic 错误信封；
// 对 key → 200。Claude Code 发的就是 x-api-key。
func TestMessagesAuthXAPIKey(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "secret"})
	const body = `{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`

	bad := httptest.NewRecorder()
	badReq := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	badReq.Header.Set("x-api-key", "wrong")
	h.ServeHTTP(bad, badReq)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("错 key code=%d want 401", bad.Code)
	}
	var errObj struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bad.Body.Bytes(), &errObj); err != nil {
		t.Fatalf("错误响应不是 JSON：%v", err)
	}
	if errObj.Type != "error" || errObj.Error.Type != "authentication_error" {
		t.Errorf("Anthropic 错误信封=%+v", errObj)
	}

	ok := httptest.NewRecorder()
	okReq := httptest.NewRequest("POST", "/messages", strings.NewReader(body)) // 双路径：/messages 也挂
	okReq.Header.Set("x-api-key", "secret")
	h.ServeHTTP(ok, okReq)
	if ok.Code != 200 {
		t.Fatalf("对 key code=%d body=%s", ok.Code, ok.Body.String())
	}
}

// TestMessagesErrorEnvelope 上游全失败：非 200 + Anthropic error 信封（不是 OpenAI 信封）。
func TestMessagesErrorEnvelope(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 500, `{"code":500,"msg":"boom"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == 200 {
		t.Fatalf("上游全失败不得回 200：%s", rec.Body.String())
	}
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("必须是 Anthropic 错误信封：%v (%s)", err, rec.Body.String())
	}
	if got.Type != "error" || got.Error.Type == "" || got.Error.Message == "" {
		t.Fatalf("错误信封不完整：%s", rec.Body.String())
	}
}

// TestMessagesAssistantTextWithToolUseSingleMessage assistant 的「正文 + tool_use」
// 必须合成一条消息（content + tool_calls）；拆成两条 consecutive assistant 会被上游拒绝。
func TestMessagesAssistantTextWithToolUseSingleMessage(t *testing.T) {
	src := `{"model":"m","max_tokens":8,"messages":[
	  {"role":"user","content":"hi"},
	  {"role":"assistant","content":[{"type":"text","text":"我来读"},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"/a"}}]}
	]}`
	raw, _, err := messagesToChat([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 {
		t.Fatalf("messages=%d want 2（user + 合并后的 assistant）：%+v", len(chat.Messages), chat.Messages)
	}
	asst := chat.Messages[1]
	if asst["role"] != "assistant" || asst["content"] != "我来读" {
		t.Fatalf("assistant 消息=%+v", asst)
	}
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("正文与 tool_calls 未合并到同一条消息：%+v", asst)
	}
}

// TestMessagesStreamErrorKeepsHTTPStatus 流式请求在首帧前失败：保留状态码 +
// Anthropic 错误信封（不能是 200 + error 事件）。
func TestMessagesStreamErrorKeepsHTTPStatus(t *testing.T) {
	p := pool.New("")
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: [DONE]\n\n", true
	})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("必须是 Anthropic 错误信封：%v (%s)", err, rec.Body.String())
	}
	if got.Type != "error" || got.Error.Type != "api_error" {
		t.Errorf("信封=%+v", got)
	}
}
