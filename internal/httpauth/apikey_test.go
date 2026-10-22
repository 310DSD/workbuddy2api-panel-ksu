// VerifyAPIKey 双头鉴权：Anthropic 的 x-api-key 与 OpenAI 的 Bearer 同口径。
package httpauth

import (
	"net/http/httptest"
	"testing"
)

func TestVerifyAPIKey(t *testing.T) {
	const key = "s3cret"
	cases := []struct {
		name   string
		authz  string
		apiKey string
		want   bool
	}{
		{"Bearer 正确", "Bearer s3cret", "", true},
		{"x-api-key 正确", "", "s3cret", true},
		{"两个都正确", "Bearer s3cret", "s3cret", true},
		{"Bearer 错 + x-api-key 对", "Bearer wrong", "s3cret", true},
		{"Bearer 对 + x-api-key 错", "Bearer s3cret", "wrong", true},
		{"两个都错", "Bearer wrong", "wrong", false},
		{"缺头", "", "", false},
		{"空 key 表示不鉴权", "", "", true}, // 见下：用独立用例
	}
	for _, c := range cases {
		if c.name == "空 key 表示不鉴权" {
			continue
		}
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		if c.authz != "" {
			r.Header.Set("Authorization", c.authz)
		}
		if c.apiKey != "" {
			r.Header.Set("x-api-key", c.apiKey)
		}
		if got := VerifyAPIKey(r, key); got != c.want {
			t.Errorf("%s: VerifyAPIKey=%v want %v", c.name, got, c.want)
		}
	}

	// api_key 未配置（空）→ 恒放行，与 VerifyBearer 同语义。
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	if !VerifyAPIKey(r, "") {
		t.Error("空 key 应恒放行")
	}
}
