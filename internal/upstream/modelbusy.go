// modelbusy.go 上游「模型繁忙度」观测（官方桌面端 POST /v2/chat/busy）。
//
// 线上实测形状（2026-10，CN 域）：
//
//	POST {chatBase}/v2/chat/busy           ← 与 /v2/chat/completions 同 base，路径必须带 /v2
//	请求: {"models": ["glm-5.3-flash", "deepseek-v4-pro"]}   ← 单次上限 50 个（超出分批）
//	响应: {"code":0,"data":[{"model":"glm-5.3-flash","busy_level":"idle"}, ...]}
//
// busy_level ∈ idle | normal | busy | congested | unavailable（实测还会出 unknown）。
// 该结果是**单次快照**：同一批重查三次可能得到 idle/unknown/idle（unknown = 服务端
// 当次无数据，不是模型不合法）。因此：
//
//   - 本文件只做读取，**绝不参与选号**（选号依据是 pool 的冷却/熔断状态）；
//   - 调用方拿不到就**不填**（unknown 不得展示成「不可用」，也不得据此改路由）；
//   - 它恰好解释了 chat 侧偶发的 429 / 6021 queue.full（同一上游容量现象的事前面）。
package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	modelBusyPath = "/v2/chat/busy"
	// modelBusyBatch 单批模型数上限：上游硬限制，超限整批被拒，故必须分批。
	modelBusyBatch = 50
)

// ModelBusy 查一批模型的繁忙度，返回 model → busy_level。
// 上游未返回的模型不在 map 里（调用方按「未知」处理）。
// 只读、无副作用；失败原样返回错误，调用方不得据此改变选号或报错给客户端。
func (c *Client) ModelBusy(a *auth.Auth, models []string) (map[string]string, error) {
	out := map[string]string{}
	for i := 0; i < len(models); i += modelBusyBatch {
		end := i + modelBusyBatch
		if end > len(models) {
			end = len(models)
		}
		raw, err := json.Marshal(map[string]any{"models": models[i:end]})
		if err != nil {
			return out, err
		}
		req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+modelBusyPath, bytes.NewReader(raw))
		if err != nil {
			return out, err
		}
		req.Header.Set("Content-Type", "application/json")
		c.ChatHeaders(req, a, "", ChatMeta{})
		data, err := c.doJSON(req)
		if err != nil {
			return out, err
		}
		var rows []struct {
			Model string `json:"model"`
			Level string `json:"busy_level"`
		}
		if err := json.Unmarshal(data, &rows); err != nil {
			return out, err
		}
		for _, r := range rows {
			if r.Model != "" && r.Level != "" {
				out[r.Model] = r.Level
			}
		}
	}
	return out, nil
}
