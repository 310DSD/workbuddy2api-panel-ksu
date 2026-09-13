// school.go 开学季活动（school-season，活动期 2026-09-13 ~ 09-24）纯 API 自动化。
//
// 判据（2026-09-13 小程序 MCP 逆向 + 三账号实测，protocol.md §7.11）：
//   - share_invite（每日 +100c +1抽奖）：POST /tasks/share-complete {channel:"wechat"}
//     即点亮——纯前端上报，服务端不校验真实分享回执。本模块的主目标。
//   - chat_3_times / expert_use：判据绑定小程序原生沙箱会话（e2b runtime），
//     webchat 普通会话不计数，纯 API 不做（需小程序内人工对话）。
//   - 抽奖：POST /wheel/draw {draw_uuid}（前端生成 uuid，消耗 1 chance）。
//
// 端点基址 www.codebuddy.cn（billing 同域）；信封 {code,msg,data}，code=0 成功。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const schoolBase = "/portal/activity/school"

// schoolJSON 学院活动 API 请求（剥信封，业务 code≠0 返回带 msg 的 error）。
func (c *Client) schoolJSON(a *auth.Auth, method, path string, body map[string]any, out any) error {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, c.BillingBaseCN+schoolBase+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// SchoolTask 开学季任务条目。
type SchoolTask struct {
	TaskCode    string `json:"task_code"`
	Status      string `json:"status"` // pending | completed | claimed
	Progress    int    `json:"progress"`
	TargetCount int    `json:"target_count"`
}

// SchoolTasks 任务列表 + 活动是否在期。
func (c *Client) SchoolTasks(a *auth.Auth) ([]SchoolTask, bool, error) {
	var out struct {
		Tasks    []SchoolTask `json:"tasks"`
		InPeriod bool         `json:"in_period"`
	}
	if err := c.schoolJSON(a, http.MethodGet, "/tasks", nil, &out); err != nil {
		return nil, false, err
	}
	return out.Tasks, out.InPeriod, nil
}

// SchoolShareComplete 上报「分享完成」（share_invite 判据，实测即点亮）。
func (c *Client) SchoolShareComplete(a *auth.Auth) error {
	return c.schoolJSON(a, http.MethodPost, "/tasks/share-complete",
		map[string]any{"channel": "wechat"}, nil)
}

// SchoolClaimTask 领取任务奖励（返回获得的抽奖次数）。
func (c *Client) SchoolClaimTask(a *auth.Auth, taskCode string) (chanceGranted int, err error) {
	var out struct {
		ChanceGranted int `json:"chance_granted"`
	}
	if err := c.schoolJSON(a, http.MethodPost, "/tasks/"+taskCode+"/claim", map[string]any{}, &out); err != nil {
		return 0, err
	}
	return out.ChanceGranted, nil
}

// SchoolChances 当前抽奖次数余额。
func (c *Client) SchoolChances(a *auth.Auth) (int, error) {
	var out struct {
		Chance struct {
			Balance int `json:"balance"`
		} `json:"chance"`
	}
	if err := c.schoolJSON(a, http.MethodGet, "/config", nil, &out); err != nil {
		return 0, err
	}
	return out.Chance.Balance, nil
}

// SchoolDraw 抽奖一次，返回奖品描述（prize_code + 积分）。
func (c *Client) SchoolDraw(a *auth.Auth) (string, error) {
	var out struct {
		PrizeCode    string `json:"prize_code"`
		CreditAmount int    `json:"credit_amount"`
	}
	if err := c.schoolJSON(a, http.MethodPost, "/wheel/draw",
		map[string]any{"draw_uuid": clientToken()}, &out); err != nil {
		return "", err
	}
	if out.CreditAmount > 0 {
		return fmt.Sprintf("%s +%dc", out.PrizeCode, out.CreditAmount), nil
	}
	return out.PrizeCode, nil
}
