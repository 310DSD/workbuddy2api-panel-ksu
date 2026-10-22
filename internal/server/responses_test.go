// responses_test.go /v1/responses 适配：请求翻译 + 出站事件流翻译 + 契约细节。
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

// respEvents 解析 Responses SSE 输出为 [(eventType, payload)]。
func respEvents(t *testing.T, body string) []struct {
	Type string
	Data map[string]any
} {
	t.Helper()
	out := []struct {
		Type string
		Data map[string]any
	}{}
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		var typ, data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				typ = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if typ == "" || data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("事件 %s 的 data 不是 JSON：%v (%s)", typ, err, data)
		}
		out = append(out, struct {
			Type string
			Data map[string]any
		}{typ, m})
	}
	return out
}

func eventTypes(evs []struct {
	Type string
	Data map[string]any
}) []string {
	types := make([]string, 0, len(evs))
	for _, e := range evs {
		types = append(types, e.Type)
	}
	return types
}

// TestResponsesToChatTranslation 请求翻译：instructions→system、input items→messages
// （含 function_call / function_call_output 配对）、reasoning.effort、tools（function
// 与 custom）、max_output_tokens→max_tokens、text.format→response_format。
func TestResponsesToChatTranslation(t *testing.T) {
	src := `{
	  "model":"cn:deepseek-v4",
	  "instructions":"你是助手",
	  "stream":true,
	  "max_output_tokens":2048,
	  "reasoning":{"effort":"high"},
	  "prompt_cache_key":"sess-1",
	  "input":[
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"北京天气"}]},
	    {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
	    {"type":"function_call_output","call_id":"call_1","output":"晴"},
	    {"type":"reasoning","summary":[{"type":"summary_text","text":"旧思考"}]}
	  ],
	  "tools":[
	    {"type":"function","name":"get_weather","description":"查天气","parameters":{"type":"object","properties":{"city":{"type":"string"}}}},
	    {"type":"custom","name":"apply_patch","description":"打补丁"},
	    {"type":"web_search"}
	  ],
	  "tool_choice":{"type":"function","name":"get_weather"},
	  "text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"},"strict":true}}
	}`
	raw, err := responsesToChat([]byte(src))
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	var chat struct {
		Model          string           `json:"model"`
		Stream         bool             `json:"stream"`
		MaxTokens      int              `json:"max_tokens"`
		ReasoningEff   string           `json:"reasoning_effort"`
		PromptCacheKey string           `json:"prompt_cache_key"`
		Messages       []map[string]any `json:"messages"`
		Tools          []map[string]any `json:"tools"`
		ToolChoice     map[string]any   `json:"tool_choice"`
		ResponseFormat map[string]any   `json:"response_format"`
	}
	if err := json.Unmarshal(raw, &chat); err != nil {
		t.Fatalf("翻译结果不是合法 JSON：%v (%s)", err, raw)
	}
	if chat.Model != "cn:deepseek-v4" || !chat.Stream || chat.MaxTokens != 2048 {
		t.Fatalf("基础字段=%+v", chat)
	}
	if chat.ReasoningEff != "high" {
		t.Errorf("reasoning.effort 未映射到 reasoning_effort：%q", chat.ReasoningEff)
	}
	if chat.PromptCacheKey != "sess-1" {
		t.Errorf("prompt_cache_key 应透传（会话粘性依赖它）：%q", chat.PromptCacheKey)
	}
	// messages: system + user + assistant(tool_calls) + tool
	if len(chat.Messages) != 4 {
		t.Fatalf("messages=%d want 4: %+v", len(chat.Messages), chat.Messages)
	}
	if chat.Messages[0]["role"] != "system" || chat.Messages[0]["content"] != "你是助手" {
		t.Errorf("instructions 未变成 system 消息：%+v", chat.Messages[0])
	}
	if chat.Messages[1]["role"] != "user" || chat.Messages[1]["content"] != "北京天气" {
		t.Errorf("input_text 未拼成 user content：%+v", chat.Messages[1])
	}
	if chat.Messages[2]["role"] != "assistant" {
		t.Errorf("function_call 应变成 assistant.tool_calls：%+v", chat.Messages[2])
	}
	tcs, _ := chat.Messages[2]["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant.tool_calls=%v", chat.Messages[2]["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("tool_call id 应沿用 call_id：%v", tc["id"])
	}
	if chat.Messages[3]["role"] != "tool" || chat.Messages[3]["tool_call_id"] != "call_1" {
		t.Errorf("function_call_output 应变成 tool 消息：%+v", chat.Messages[3])
	}
	// tools: function + custom（web_search 丢弃）
	if len(chat.Tools) != 2 {
		t.Fatalf("tools=%d want 2（托管工具应丢弃）：%+v", len(chat.Tools), chat.Tools)
	}
	if chat.Tools[0]["type"] != "function" || chat.Tools[0]["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("function 工具翻译错：%+v", chat.Tools[0])
	}
	custom := chat.Tools[1]["function"].(map[string]any)
	if custom["name"] != "apply_patch" {
		t.Errorf("custom 工具应折叠成 function：%+v", chat.Tools[1])
	}
	props := custom["parameters"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["input"]; !ok {
		t.Errorf("custom 工具应有单参 input：%+v", custom["parameters"])
	}
	if chat.ToolChoice["type"] != "function" ||
		chat.ToolChoice["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tool_choice 翻译错：%+v", chat.ToolChoice)
	}
	if chat.ResponseFormat["type"] != "json_schema" {
		t.Errorf("text.format 未映射到 response_format：%+v", chat.ResponseFormat)
	}
}

// TestResponsesNonStream 非流式：chat.completion → response 对象（output/reasoning/usage）。
func TestResponsesNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"想一想\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\n" +
			"data: [DONE]\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	const body = `{"model":"glm-5.2","input":"hi","max_output_tokens":64}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object string           `json:"object"`
		Status string           `json:"status"`
		Model  string           `json:"model"`
		Output []map[string]any `json:"output"`
		Usage  map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%v (%s)", err, rec.Body.String())
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Fatalf("object/status=%s/%s", resp.Object, resp.Status)
	}
	if resp.Model != "glm-5.2" {
		t.Errorf("model 应回显裸名（去 cn: 前缀）：%q", resp.Model)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output=%d want 2（reasoning + message）：%+v", len(resp.Output), resp.Output)
	}
	if resp.Output[0]["type"] != "reasoning" {
		t.Errorf("首个 item 应为 reasoning：%+v", resp.Output[0])
	}
	if resp.Output[1]["type"] != "message" {
		t.Errorf("第二个 item 应为 message：%+v", resp.Output[1])
	}
	content := resp.Output[1]["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" || content["text"] != "你好" {
		t.Errorf("message content=%+v", content)
	}
	if resp.Usage["input_tokens"].(float64) != 11 || resp.Usage["output_tokens"].(float64) != 7 {
		t.Errorf("usage=%+v", resp.Usage)
	}
	details, _ := resp.Usage["input_tokens_details"].(map[string]any)
	if details == nil || details["cached_tokens"].(float64) != 3 {
		t.Errorf("usage.input_tokens_details=%+v（Codex 据此显示 cached）", resp.Usage)
	}
}

// TestResponsesStreamEvents 流式：事件序列 + sequence_number 单调递增 + 终态恰一个。
func TestResponsesStreamEvents(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"思考\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
			"data: [DONE]\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	const body = `{"model":"glm-5.2","input":"hi","stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))

	evs := respEvents(t, rec.Body.String())
	types := eventTypes(evs)
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",  // reasoning done
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done", // message done
		"response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列不符：\n got=%v\nwant=%v", types, want)
	}
	// sequence_number 严格递增（Codex 依赖它排序/去重）。
	prev := -1.0
	for i, e := range evs {
		v, ok := e.Data["sequence_number"].(float64)
		if !ok {
			t.Fatalf("第 %d 个事件 %s 缺 sequence_number", i, e.Type)
		}
		if v <= prev {
			t.Fatalf("sequence_number 未递增：%v 之后是 %v（第 %d 个事件）", prev, v, i)
		}
		prev = v
	}
	// 终态只出现一次，带 usage 与 output。
	last := evs[len(evs)-1]
	if last.Type != "response.completed" {
		t.Fatalf("终态事件=%s", last.Type)
	}
	respObj := last.Data["response"].(map[string]any)
	if respObj["status"] != "completed" {
		t.Errorf("response.status=%v", respObj["status"])
	}
	if out, _ := respObj["output"].([]any); len(out) != 2 {
		t.Errorf("completed.output=%v", respObj["output"])
	}
	if u, _ := respObj["usage"].(map[string]any); u["total_tokens"].(float64) != 7 {
		t.Errorf("completed.usage=%v", respObj["usage"])
	}
}

// TestResponsesStreamToolCalls 工具调用分片：function_call item 的 added / 参数增量 /
// 参数完成 / item 完成，且 output 里有 function_call。
func TestResponsesStreamToolCalls(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"北京\\\"}\"}}]}}]}\n\n" +
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	const body = `{"model":"glm-5.2","input":"hi","stream":true,"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))

	evs := respEvents(t, rec.Body.String())
	var added, argDeltas, argDone, itemDone int
	for _, e := range evs {
		switch e.Type {
		case "response.output_item.added":
			if item, _ := e.Data["item"].(map[string]any); item["type"] == "function_call" {
				added++
				if item["name"] != "get_weather" || item["call_id"] != "call_a" {
					t.Errorf("function_call item=%+v", item)
				}
			}
		case "response.function_call_arguments.delta":
			argDeltas++
		case "response.function_call_arguments.done":
			argDone++
			if e.Data["arguments"] != `{"city":"北京"}` {
				t.Errorf("累积参数=%v", e.Data["arguments"])
			}
		case "response.output_item.done":
			if item, _ := e.Data["item"].(map[string]any); item["type"] == "function_call" {
				itemDone++
				if item["status"] != "completed" {
					t.Errorf("item.status=%v", item["status"])
				}
			}
		}
	}
	if added != 1 || argDeltas != 2 || argDone != 1 || itemDone != 1 {
		t.Fatalf("added=%d deltas=%d done=%d itemDone=%d", added, argDeltas, argDone, itemDone)
	}
	if evs[len(evs)-1].Type != "response.completed" {
		t.Fatalf("终态=%s", evs[len(evs)-1].Type)
	}
}

// TestResponsesWebsocketUnsupported GET /v1/responses 的 WS 升级探测回 426：
// 客户端据此立刻回落 HTTPS（否则要空转重试）。
func TestResponsesWebsocketUnsupported(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, "{}", false })})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/responses", nil))
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("code=%d want 426", rec.Code)
	}
	if rec.Header().Get("Upgrade") != "websocket" {
		t.Errorf("Upgrade 头=%q", rec.Header().Get("Upgrade"))
	}
}

// TestResponsesErrorPassthrough 上游错误（402 余额不足）原样透出状态码与 error 信封：
// 翻译错误会丢掉 chat 管道写好的 code / gateway_hint。
func TestResponsesErrorPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`)))
	// 402 被 chat 管道换算成硬冷却后换号，最终无可用账号 → 503/429 之一；关键是必须是
	// JSON error 信封且带 code，而不是被翻译成 response 对象。
	if rec.Code == 200 {
		t.Fatalf("上游全失败时不得回 200：%s", rec.Body.String())
	}
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("错误响应必须是 JSON error 信封：%v (%s)", err, rec.Body.String())
	}
	if got.Error.Code == "" {
		t.Errorf("error.code 不能为空：%s", rec.Body.String())
	}
}

// TestResponsesStreamErrorKeepsHTTPStatus 流式请求在首帧前失败（无可用账号）：
// 必须保留原 HTTP 状态码与 JSON 错误信封，不能伪装成「200 + response.failed」——
// Codex 依赖 HTTP 码决定重试/换模型。
func TestResponsesStreamErrorKeepsHTTPStatus(t *testing.T) {
	p := pool.New("") // 空池：选号必失败
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: [DONE]\n\n", true
	})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503（流式失败必须保留 HTTP 状态）body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type=%q want application/json（不是 SSE）", rec.Header().Get("Content-Type"))
	}
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body 必须是 JSON 错误信封：%v (%s)", err, rec.Body.String())
	}
	if got.Error.Code != "no_healthy_account" {
		t.Errorf("code=%q want no_healthy_account", got.Error.Code)
	}
}
