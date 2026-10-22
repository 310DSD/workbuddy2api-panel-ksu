package server

import "testing"

// TestIsInferencePath 推理入口白名单：三个协议四个路径（含 /messages 别名）都要
// 进请求级观测包装；其他路径不得被误包（否则每 5 秒的 overview 轮询也会写归档）。
func TestIsInferencePath(t *testing.T) {
	for _, p := range []string{
		"/v1/chat/completions",
		"/v1/responses",
		"/v1/messages",
		"/messages", // Anthropic 别名（审查 M2：曾漏出观测）
	} {
		if !isInferencePath(p) {
			t.Errorf("isInferencePath(%q)=false want true", p)
		}
	}
	for _, p := range []string{
		"/v1/models",
		"/healthz",
		"/status",
		"/panel/api/overview",
		"/v1/messages/extra",
		"",
	} {
		if isInferencePath(p) {
			t.Errorf("isInferencePath(%q)=true want false", p)
		}
	}
}
