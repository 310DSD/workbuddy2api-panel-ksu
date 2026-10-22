// modelunavail.go 整体不可用模型（model-wide unavailable）的判定与探针窗口。
//
// 背景（对应上游 issue #102 的下一层问题）：11102「该后端无此模型」原本是**逐账号**
// 负缓存——模型整体下架时，每个号都要白撞一次上游，这段时间里 `/v1/models` 照旧
// 列出它，客户端选到就一路 503/400，运维却只能看到「一个号一个号地在冷却」。
//
// 提升判据（只看上游阳性证据，绝不从「远端目录里没有」反推——目录滞后/域差异太常见）：
//
//	同一 realm 有 **>= 2 个不同账号** 都被上游明确回 11102 → 判模型整体不可用。
//
// 为什么是 2：单号命中可能只是「这个号没有这个模型」（号型/权限差异，见 11158），
// 两个独立账号同时被同一模型拒绝，才足以排除账号个体原因。11158「号型不允许」是
// 账号权限差异，**不参与**提升。
//
// 判据完全**派生**自各账号持久化的 11102 台账（modelCooldowns，reason 前缀 "11102"）：
// 不引入新的持久状态，重启后证据仍在；任一账号在该模型上实测成功（BlockModelClear）
// 台账即消失，计数跌破阈值 → 自动恢复，不需要任何人工复位。
//
// 探针窗口：判定为整体不可用后不再对每个请求打上游（那是纯浪费），但也不能永远拒——
// 上游可能悄悄恢复。每 modelUnavailProbeGap 放行一个真请求去探，失败会重新入册、
// 成功则清账，两个方向都能自愈。
package pool

import (
	"sort"
	"strings"
	"time"
)

const (
	// modelUnavailMinAccounts 提升门槛：>=2 个不同账号被明确回 11102。
	modelUnavailMinAccounts = 2
	// modelUnavailProbeGap 判定后允许放行的探针间隔。取 30m：比上游目录刷新（1h）
	// 更快，又不至于让一个真下架的模型每次请求都白打一次上游。
	modelUnavailProbeGap = 30 * time.Minute
	// modelUnavailReasonPrefix 11102 台账的 reason 前缀（与 BlockModelClear 同口径）。
	modelUnavailReasonPrefix = "11102"
)

// ModelUnavailable 描述一个「整体不可用」的模型（面板/状态接口消费）。
type ModelUnavailable struct {
	Realm string `json:"realm,omitempty"`
	Model string `json:"model"`
	// Accounts 命中 11102 的不同账号数（>= modelUnavailMinAccounts 才成立）。
	Accounts int `json:"accounts"`
	// Reason 上游原文（台账里存的第一条，含 code=11102 与模型名）。
	Reason string `json:"reason,omitempty"`
	// Until 最早解封时刻（各账号该模型负缓存的最早到期时间；零值 = 未记录）。
	Until time.Time `json:"until,omitempty"`
}

func unavailKey(realm, model string) string { return realm + "\x1f" + model }

// modelUnavailCountLocked 统计 (realm, model) 上被明确回 11102 的不同账号数，
// 返回计数、第一条上游原因与最早解封时刻。禁用/暂停账号不计（它们的不可用与模型无关，
// 也不该支撑「模型整体下架」的结论）。realm 为空 = 跨域合计。
// 调用方须持 p.mu（读 e.modelCooldowns / e.a）。
func (p *Pool) modelUnavailCountLocked(realm, model string, now time.Time) (int, string, time.Time) {
	if model == "" {
		return 0, "", time.Time{}
	}
	n := 0
	reason := ""
	var until time.Time
	for _, e := range p.byUID {
		if e.disabled || e.paused {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		mc, ok := e.modelCooldowns[model]
		if !ok || !strings.HasPrefix(mc.Reason, modelUnavailReasonPrefix) {
			continue
		}
		if !mc.Until.IsZero() && !now.Before(mc.Until) {
			continue // 已到期：不再是证据
		}
		n++
		if reason == "" {
			reason = mc.Reason
		}
		if !mc.Until.IsZero() && (until.IsZero() || mc.Until.Before(until)) {
			until = mc.Until
		}
	}
	return n, reason, until
}

// ModelUnavailableFor 报告 (realm, model) 是否已判「整体不可用」，并处理探针窗口。
//
// 返回值第二项 blocked=true 表示本次请求应当**在打上游之前**就拒掉（400
// model_unavailable）；blocked=false 且 info.Model 非空表示「已判定，但本次是探针
// 请求，放行一次去验证上游是否恢复」。
//
// realm 传空 = 跨域合计（调用方未指定域时，任一域的两个号也足以证明该模型整体下架）。
func (p *Pool) ModelUnavailableFor(realm, model string) (ModelUnavailable, bool) {
	if model == "" {
		return ModelUnavailable{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n, reason, until := p.modelUnavailCountLocked(realm, model, now)
	if n < modelUnavailMinAccounts {
		// 证据不足：顺手清掉探针记录（模型已恢复，下次再判要从「立即放行」重新开始）。
		delete(p.unavailProbe, unavailKey(realm, model))
		return ModelUnavailable{}, false
	}
	info := ModelUnavailable{Realm: realm, Model: model, Accounts: n, Reason: reason, Until: until}
	key := unavailKey(realm, model)
	if last, ok := p.unavailProbe[key]; ok && now.Sub(last) < modelUnavailProbeGap {
		return info, true // 探针窗口内：拒（不打上游）
	}
	p.unavailProbe[key] = now // 放行本次探针
	return info, false
}

// UnavailableModels 返回当前被判「整体不可用」的全部模型（按域 + 模型名稳定排序）。
// 供 /v1/models 过滤与面板展示；只读，不推进探针窗口。
func (p *Pool) UnavailableModels() []ModelUnavailable {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	// 先收集出现过的 (realm, model) 组合：realm 取账号所属域。
	type key struct{ realm, model string }
	seen := map[key]bool{}
	for _, e := range p.byUID {
		if e.disabled || e.paused {
			continue
		}
		for model, mc := range e.modelCooldowns {
			if !strings.HasPrefix(mc.Reason, modelUnavailReasonPrefix) {
				continue
			}
			if !mc.Until.IsZero() && !now.Before(mc.Until) {
				continue
			}
			seen[key{e.a.Realm(), model}] = true
		}
	}
	out := make([]ModelUnavailable, 0, len(seen))
	for k := range seen {
		n, reason, until := p.modelUnavailCountLocked(k.realm, k.model, now)
		if n < modelUnavailMinAccounts {
			continue
		}
		out = append(out, ModelUnavailable{Realm: k.realm, Model: k.model, Accounts: n, Reason: reason, Until: until})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Realm != out[j].Realm {
			return out[i].Realm < out[j].Realm
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// UnavailableModelSet 返回 "realm:model" 形式的判定集合（/v1/models 过滤用）。
func (p *Pool) UnavailableModelSet() map[string]bool {
	list := p.UnavailableModels()
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]bool, len(list))
	for _, u := range list {
		out[u.Realm+":"+u.Model] = true
	}
	return out
}
