// modelunavail_test.go 整体不可用模型：提升判据（>=2 个不同账号被上游明确回 11102）、
// 探针窗口、以及「只凭阳性证据、绝不由目录缺失反推」的边界。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// unavailPool 造一个带 n 个 cn 账号的池。
func unavailPool(uids ...string) *Pool {
	p := New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", Domain: "www.codebuddy.cn"})
	}
	return p
}

// TestModelUnavailableNeedsTwoAccounts 单号 11102 不提升：可能只是「这个号没这个模型」。
// 两个不同账号都被明确拒绝才判模型整体下架。
func TestModelUnavailableNeedsTwoAccounts(t *testing.T) {
	p := unavailPool("a", "b", "c")
	p.BlockModelBackoff("a", "ghost-model", "11102 model not available")
	if _, blocked, _ := p.ModelUnavailableFor("cn", "ghost-model"); blocked {
		t.Fatal("单号命中不得提升为整体不可用")
	}
	if got := p.UnavailableModels(); len(got) != 0 {
		t.Fatalf("单号命中时清单应为空，got %+v", got)
	}

	p.BlockModelBackoff("b", "ghost-model", "11102 model not available")
	list := p.UnavailableModels()
	if len(list) != 1 || list[0].Model != "ghost-model" || list[0].Realm != "cn" || list[0].Accounts != 2 {
		t.Fatalf("两号命中应提升为整体不可用，got %+v", list)
	}
	if !p.UnavailableModelSet()["cn:ghost-model"] {
		t.Fatal("UnavailableModelSet 应含 cn:ghost-model")
	}
}

// TestModelUnavailableProbeWindow 判定后：首个请求放行探针，其后窗口内一律拦
// （每 30 分钟只放一个真请求去验证上游是否恢复）。
func TestModelUnavailableProbeWindow(t *testing.T) {
	p := unavailPool("a", "b")
	p.BlockModelBackoff("a", "ghost", "11102 model not available")
	p.BlockModelBackoff("b", "ghost", "11102 model not available")

	// 第一次：放行探针（blocked=false，但仍返回 info）。名额在真正打上游前才记账（M7）。
	info, blocked, probe := p.ModelUnavailableFor("cn", "ghost")
	if blocked || !probe {
		t.Fatal("判定后的首个请求应放行探针（否则上游恢复了永远学不到）")
	}
	// 未记账前重复调用仍是探针（说明没触达上游的请求不吃名额）。
	if _, blocked2, probe2 := p.ModelUnavailableFor("cn", "ghost"); blocked2 || !probe2 {
		t.Fatal("未真正发上游请求时不应消耗探针名额（M7）")
	}
	p.MarkModelUnavailableProbe("cn", "ghost")
	// 记账后立刻再请求：拦。
	if _, blocked3, _ := p.ModelUnavailableFor("cn", "ghost"); !blocked3 {
		t.Fatal("探针后窗口内应拦截（不再每次请求都打上游）")
	}
	if info.Accounts != 2 {
		t.Errorf("Accounts=%d want 2", info.Accounts)
	}
	// 窗口过期后再次放行。
	p.mu.Lock()
	p.unavailProbe[unavailKey("cn", "ghost")] = time.Now().Add(-modelUnavailProbeGap - time.Second)
	p.mu.Unlock()
	if _, blocked, _ := p.ModelUnavailableFor("cn", "ghost"); blocked {
		t.Fatal("探针窗口过期后应再放行一次")
	}
}

// TestModelUnavailableIgnores11158 11158「号型不允许」是账号权限差异，不参与提升：
// 否则一个没有某模型权限的小号集群会把该模型在整个域里判死。
func TestModelUnavailableIgnores11158(t *testing.T) {
	p := unavailPool("a", "b")
	p.BlockModelBackoff("a", "pro-model", "11158 account type not allowed")
	p.BlockModelBackoff("b", "pro-model", "11158 account type not allowed")
	if _, blocked, _ := p.ModelUnavailableFor("cn", "pro-model"); blocked {
		t.Fatal("11158 不得触发整体不可用提升")
	}
	if got := p.UnavailableModels(); len(got) != 0 {
		t.Fatalf("清单应为空，got %+v", got)
	}
}

// TestModelUnavailableAutoRecovers 任一账号实测成功（BlockModelClear 清 11102 台账）
// 后计数跌破阈值即自动恢复，无需人工复位。
func TestModelUnavailableAutoRecovers(t *testing.T) {
	p := unavailPool("a", "b")
	p.BlockModelBackoff("a", "ghost", "11102 model not available")
	p.BlockModelBackoff("b", "ghost", "11102 model not available")
	if len(p.UnavailableModels()) != 1 {
		t.Fatal("前置失效：应已提升")
	}
	p.BlockModelClear("a", "ghost")
	if got := p.UnavailableModels(); len(got) != 0 {
		t.Fatalf("清掉一个账号的台账后应恢复，got %+v", got)
	}
	if _, blocked, _ := p.ModelUnavailableFor("cn", "ghost"); blocked {
		t.Fatal("证据不足时不得拦截")
	}
}

// TestModelUnavailableSkipsDisabledAndPaused 禁用/暂停号的台账不构成证据：
// 它们本就不参与选号，把它们算进去会让「停了两个号」看起来像「模型下架」。
func TestModelUnavailableSkipsDisabledAndPaused(t *testing.T) {
	p := unavailPool("a", "b", "c")
	p.BlockModelBackoff("a", "ghost", "11102 model not available")
	p.BlockModelBackoff("b", "ghost", "11102 model not available")
	p.Disable("a", "manual")
	p.Pause("b")
	if got := p.UnavailableModels(); len(got) != 0 {
		t.Fatalf("禁用/暂停号的证据应被忽略，got %+v", got)
	}
}

// TestModelUnavailableRealmScoped 分域判定：cn 域只有 1 个号命中、global 有 1 个命中
// → 各域都不到 2，不算；但未指定域（跨域合计）时两号足以判定。
func TestModelUnavailableRealmScoped(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	// global 域的判定走 auth 的 globalEnabled 逃生门 + workbuddy.ai 域后缀（同 realm_test）。
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "at", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"})
	p.BlockModelBackoff("cn1", "ghost", "11102 model not available")
	p.BlockModelBackoff("g1", "ghost", "11102 model not available")

	if _, blocked, _ := p.ModelUnavailableFor("cn", "ghost"); blocked {
		t.Fatal("cn 域只有 1 个号命中，不应拦截该域的请求")
	}
	// 跨域合计 2 个号：首个请求是探针（放行），记账后第二次才拦。
	if _, blocked, probe := p.ModelUnavailableFor("", "ghost"); blocked || !probe {
		t.Fatal("跨域合计 >=2 个账号命中时应放行探针")
	}
	p.MarkModelUnavailableProbe("", "ghost")
	if _, blocked, _ := p.ModelUnavailableFor("", "ghost"); !blocked {
		t.Fatal("跨域合计 >=2 个账号命中时应拦截（第二次请求）")
	}
}

// TestModelLockViewMarksUnavailable 面板「模型锁池」表：整体不可用的模型状态为
// unavailable 且排在最前（与 locked「等就有」区分开）。
func TestModelLockViewMarksUnavailable(t *testing.T) {
	p := unavailPool("a", "b", "c")
	p.BlockModelBackoff("a", "ghost", "11102 model not available")
	p.BlockModelBackoff("b", "ghost", "11102 model not available")
	p.CooldownSoftForModel("c", time.Minute, time.Now().Add(time.Hour), "busy-model", "6004 model rate limit")

	rows := p.ModelLockView()
	if len(rows) == 0 {
		t.Fatal("ModelLockView 应有行")
	}
	if rows[0].Model != "ghost" || rows[0].State != "unavailable" {
		t.Fatalf("首行应为 ghost/unavailable，got %+v", rows[0])
	}
	var busy *ModelLockRow
	for i := range rows {
		if rows[i].Model == "busy-model" {
			busy = &rows[i]
		}
	}
	if busy == nil || busy.State != "partial" {
		t.Fatalf("busy-model 应为 partial（仍有一个号能服务），got %+v", busy)
	}
}
