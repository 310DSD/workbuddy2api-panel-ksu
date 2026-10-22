// compensation.go 官方补偿活动的「先查后领 + 截止倒计时」。
//
// 为什么不能盲领：无活动 / 已领过时上游仍回 code=0 且 body 无 credit，领取函数返回
// (0, nil)——日志里多一行假的「+0c」成功行，真正的失败（401/5xx/网络）反倒无痕。
// 补偿带领取截止日，错过就静默过期，所以面板要把「有没有、还剩多久」摆出来。
//
//	GET  /panel/api/compensation                 逐号查状态（只读，不领）
//	POST /panel/api/accounts/{uid}/compensation  单号：查 → 有待领才领
//	POST /panel/api/compensation_all             全量：逐号 查 → 领
//
// 查询失败的账号**照样占一行**（error 字段），不把「查不到」显示成「没有」——
// 两者的处置完全不同（前者去查网络/凭证，后者什么都不用做）。
package panel

import (
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// compensationEndLayout 上游 end_time 的形态（无时区，按宿主本地时区解释）。
const compensationEndLayout = "2006-01-02 15:04:05"

// compensationRow 单个账号的补偿状态行（面板消费）。
type compensationRow struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname,omitempty"`
	Realm      string `json:"realm,omitempty"`
	Enterprise bool   `json:"enterprise,omitempty"`
	Disabled   bool   `json:"disabled,omitempty"`
	Active     bool   `json:"active"`
	Claimed    bool   `json:"claimed"`
	CreditNum  int64  `json:"credit_num"`
	EndTime    string `json:"end_time,omitempty"`
	// SecondsLeft 距领取截止的秒数（精确值，前端用 dur() 渲染成「x时y分」）；
	// end_time 缺失 / 解析失败 / 已过期恒为 0。≤48h 由前端标红（补偿通常只挂几天）。
	SecondsLeft int64 `json:"seconds_left"`
	// Expired 领取截止已过。单列一个布尔而不是靠 seconds_left 的负值推断：
	// 「未知」与「已过期」的处置完全不同，粘在一个数字里前端必然误读。
	Expired bool   `json:"expired,omitempty"`
	Error   string `json:"error,omitempty"`
	// Pending 可领且未领（前端据此显示「领取」按钮，后端同一判据）。
	Pending bool `json:"pending"`
}

// compensationRow 查询单账号状态并组装成一行。任何查询错误都收敛进 Error 字段，
// 不让一个坏号把整张卡片打成 502。
func (p *Panel) compensationRow(a *auth.Auth, st pool.Status) compensationRow {
	row := compensationRow{
		UID:        st.UID,
		Nickname:   st.Nickname,
		Realm:      st.Realm,
		Enterprise: st.Enterprise,
		Disabled:   st.Disabled,
	}
	if a == nil || a.AccessTokenValue() == "" {
		row.Error = "账号无可用凭证"
		return row
	}
	cs, err := p.cfg.Upstream.GetCompensationStatus(a)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.Active = cs.Active
	row.Claimed = cs.Claimed
	row.CreditNum = cs.CreditNum
	row.EndTime = cs.EndTime
	row.Pending = cs.Pending()
	if cs.EndTime != "" {
		if t, perr := time.ParseInLocation(compensationEndLayout, cs.EndTime, time.Local); perr == nil {
			if t.Before(time.Now()) {
				row.Expired = true
			} else {
				row.SecondsLeft = int64(time.Until(t).Seconds())
			}
		}
	}
	return row
}

// compensationList GET /panel/api/compensation：逐号查状态（只读）。
func (p *Panel) compensationList(w http.ResponseWriter, r *http.Request) {
	statuses := p.cfg.Pool.List()
	rows := make([]compensationRow, 0, len(statuses))
	pending, failed := 0, 0
	for _, st := range statuses {
		row := p.compensationRow(p.cfg.Pool.AuthByUID(st.UID), st)
		if row.Error != "" {
			failed++
		} else if row.Pending {
			pending++
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": rows, "pending": pending, "failed": failed,
	})
}

// compensationClaim POST /panel/api/accounts/{uid}/compensation：单号「查 → 领」。
// 无待领返回 200 + claimed:false + reason（不是错误：无活动/已领过都是正常状态）。
func (p *Panel) compensationClaim(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	st, ok := p.cfg.Pool.Status(uid)
	if !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	row := p.compensationRow(a, st)
	if row.Error != "" {
		writeErr(w, http.StatusBadGateway, "compensation status: "+row.Error)
		return
	}
	if !row.Pending {
		reason := "no_activity" // active=false 或 credit_num<=0
		if row.Active && row.Claimed {
			reason = "already_claimed"
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "claimed": false, "reason": reason, "status": row})
		return
	}
	credit, err := p.cfg.Upstream.ClaimCompensation(a)
	if err != nil {
		log.Printf("panel: compensation claim uid=%s 失败：%v", logfmt.Label(uid, row.Nickname), err)
		writeErr(w, http.StatusBadGateway, "claim compensation: "+err.Error())
		return
	}
	log.Printf("panel: compensation claim uid=%s 🎊 +%dc（截止 %s）", logfmt.Label(uid, row.Nickname), credit, row.EndTime)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "claimed": true, "credit": credit})
}

// compensationClaimAll POST /panel/api/compensation_all：逐号 查 → 领（幂等）。
// 单号失败不影响其他号：结果逐条返回，前端汇总「领取 N 个 / 无待领 M 个 / 失败 K 个」。
func (p *Panel) compensationClaimAll(w http.ResponseWriter, r *http.Request) {
	statuses := p.cfg.Pool.List()
	type result struct {
		UID    string `json:"uid"`
		Credit int64  `json:"credit"`
		Reason string `json:"reason,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(statuses))
	claimed, skipped, failed := 0, 0, 0
	for _, st := range statuses {
		if st.Disabled {
			// 禁用号（授权终态/人工停用）不参与批量领取：打上游必然 401，只会把
			// failed 计数刷成噪音；它们的补偿状态在列表接口里仍如实展示（人工处置）。
			skipped++
			results = append(results, result{UID: st.UID, Reason: "disabled"})
			continue
		}
		row := p.compensationRow(p.cfg.Pool.AuthByUID(st.UID), st)
		switch {
		case row.Error != "":
			failed++
			results = append(results, result{UID: st.UID, Error: row.Error})
		case !row.Pending:
			skipped++
			reason := "no_activity"
			if row.Active && row.Claimed {
				reason = "already_claimed"
			}
			results = append(results, result{UID: st.UID, Reason: reason})
		default:
			a := p.cfg.Pool.AuthByUID(st.UID)
			credit, err := p.cfg.Upstream.ClaimCompensation(a)
			if err != nil {
				failed++
				results = append(results, result{UID: st.UID, Error: err.Error()})
				continue
			}
			claimed++
			results = append(results, result{UID: st.UID, Credit: credit})
			log.Printf("panel: compensation claim_all uid=%s 🎊 +%dc", logfmt.Label(st.UID, row.Nickname), credit)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "claimed": claimed, "skipped": skipped, "failed": failed, "results": results,
	})
}
