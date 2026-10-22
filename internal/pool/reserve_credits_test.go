// 保留积分（reserve credits）：与 credit floor 互补的**账号级保命线**。
//
// credit_floor 只拦「已证收费」的模型（tier 0/1 放行，给新模型留学习机会）；
// reserve_credits 在余额**已知**且 <= 阈值时对任何模型停牌，唯一豁免是「已确认免费」
// 的模型。它堵的正是 floor 有意放过的那个洞：无观测的高价新模型把见底号一次打穿到 0
// （上游余额归零会发提醒短信）。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReserveCreditsParksUnknownTierAccount reserve 与 floor 的关键差异：
// tier 1（无观测、目录未覆盖）模型下 credit_floor 放行，但 reserve 停牌。
func TestReserveCreditsParksUnknownTierAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.Add(&auth.Auth{UID: "rich"})
	p.SetCredits("poor", 5, 1000)     // 余额已知：5 <= 10
	p.SetCredits("rich", 5000, 10000) // 余额充足
	// 无任何 NoteModelCost / 目录倍率：对双方都是 tier 1（未知）。

	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "brand-new-model", "")
		if a == nil {
			t.Fatalf("第 %d 次选号返回 nil，want rich（见底号应停牌，rich 承接）", i)
		}
		if a.UID == "poor" {
			t.Fatalf("第 %d 次选中 poor（credits=5 <= reserve=10），保留积分应停牌", i)
		}
	}
}

// TestReserveCreditsOnlyBlocksKnownBalance 余额未知（creditsTotal==0，从未查到余额）
// 不拦：否则全新装机会看起来整池不可用。
func TestReserveCreditsOnlyBlocksKnownBalance(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "fresh"})
	p.SetCredits("fresh", 5, 0) // credits 有值但总额未知（旧 state / 查询失败）

	a := p.PickExcludingForRealm(nil, "any-model", "")
	if a == nil {
		t.Fatal("余额未知的账号不应被保留积分停牌，got nil")
	}
	if a.UID != "fresh" {
		t.Fatalf("选中 %v，want fresh", a.UID)
	}
}

// TestReserveCreditsOffByDefault 阈值 0 = 关闭：行为与未开该功能完全一致。
func TestReserveCreditsOffByDefault(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	if got := p.ReserveCredits(); got != 0 {
		t.Fatalf("默认 ReserveCredits()=%d，want 0（关闭）", got)
	}
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 3, 1000)

	if a := p.PickExcludingForRealm(nil, "any-model", ""); a == nil || a.UID != "poor" {
		t.Fatalf("阈值 0 时见底号应照常接单，got %v", a)
	}
}

// TestReserveCreditsSetNegativeIgnored 负值非法：保留现值（与 SetCreditFloor 同风格）。
func TestReserveCreditsSetNegativeIgnored(t *testing.T) {
	p := New("")
	p.SetReserveCredits(10)
	p.SetReserveCredits(-1)
	if got := p.ReserveCredits(); got != 10 {
		t.Fatalf("负值应被忽略，ReserveCredits()=%d want 10", got)
	}
	p.SetReserveCredits(0) // 0 是合法值（关闭）
	if got := p.ReserveCredits(); got != 0 {
		t.Fatalf("0 应生效（关闭），ReserveCredits()=%d want 0", got)
	}
}

// TestReserveCreditsAllowsConfirmedFreeModel 已确认免费的模型（实测 cost<=0）仍可命中：
// 见底号要把最后额度留给免费档。免费档本身不扣积分，不违背保命目的。
func TestReserveCreditsAllowsConfirmedFreeModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 5, 1000)
	p.NoteModelCost("poor", "free-model", 0, 1000) // 实测免费 → tier 0

	a := p.PickExcludingForRealm(nil, "free-model", "")
	if a == nil {
		t.Fatal("已确认免费的模型不应被保留积分停牌，got nil")
	}
	if a.UID != "poor" {
		t.Fatalf("选中 %v，want poor", a.UID)
	}
}

// TestReserveCreditsFreeByCatalogRate 无实测观测，但上游目录倍率为 0 → 同样豁免：
// 免费判据与 floor 同口径（台账优先，目录兜底），不能因为「没学过」就停牌免费档。
func TestReserveCreditsFreeByCatalogRate(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetModelRateOf(func(realm, model string) string {
		if model == "free-by-rate" {
			return "0.00"
		}
		return ""
	})
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 5, 1000)

	if a := p.PickExcludingForRealm(nil, "free-by-rate", ""); a == nil {
		t.Fatal("目录倍率 0 的模型应豁免保留积分，got nil")
	}
	// 同一账号对倍率未知的模型仍然停牌（对照）。
	if a := p.PickExcludingForRealm(nil, "unknown-rate", ""); a != nil {
		t.Fatalf("倍率未知的模型应停牌，got %v", a.UID)
	}
}

// TestReserveCreditsAllParkedReturnsNil 全池停牌时选号返回 nil（含兜底路径），
// 不能把见底号从冷却兜底里「捞回来」——否则停牌形同虚设。
func TestReserveCreditsAllParkedReturnsNil(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)
	for _, uid := range []string{"a", "b"} {
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, 5, 1000)
	}
	if a := p.PickExcludingForRealm(nil, "any-model", ""); a != nil {
		t.Fatalf("全池停牌，want nil，got %v", a.UID)
	}
}

// TestReserveCreditsStickyUnbind 粘性路径同判据：绑定号见底后返回 nil，
// handler 据此解绑换号（避免钉在见底号上直到轮换耗尽）。
func TestReserveCreditsStickyUnbind(t *testing.T) {
	p := New("")
	p.SetReserveCredits(10)
	p.Add(&auth.Auth{UID: "sticky"})
	p.SetCredits("sticky", 100, 1000)

	if a := p.PickByUIDForModel("sticky", "any-model"); a == nil {
		t.Fatal("余额充足时粘性命中应放行")
	}
	// 余额被消耗到阈值以下（余额已知）。
	p.SetCredits("sticky", 5, 1000)
	if a := p.PickByUIDForModel("sticky", "any-model"); a != nil {
		t.Fatalf("见底号粘性命中应返回 nil（解绑换号），got %v", a.UID)
	}
	// 已确认免费的模型仍放行。
	p.NoteModelCost("sticky", "free-model", 0, 1000)
	if a := p.PickByUIDForModel("sticky", "free-model"); a == nil {
		t.Fatal("已确认免费模型的粘性命中应放行")
	}
}

// TestReserveCreditsRecoversAfterTopUp 充值回血（SetCreditsDetailed 刷新权威余额）
// 越过阈值即刻恢复，无需任何复位操作。
func TestReserveCreditsRecoversAfterTopUp(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetReserveCredits(10)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "only"})
	p.SetCredits("only", 5, 1000)

	if a := p.PickExcludingForRealm(nil, "any-model", ""); a != nil {
		t.Fatalf("见底期应停牌，got %v", a.UID)
	}
	p.SetCreditsDetailed("only", 500, 1000, 0, time.Time{}, 0)
	if a := p.PickExcludingForRealm(nil, "any-model", ""); a == nil {
		t.Fatal("充值回血后应自动恢复接单，got nil")
	}
}

// TestReserveCreditsStatusMarker 面板标记：余额已知且 <= 阈值时 Status.Reserved 为真，
// 关闭（0）或余额未知时为假。
func TestReserveCreditsStatusMarker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 5, 1000)

	st, ok := p.Status("poor")
	if !ok {
		t.Fatal("Status(poor) 不存在")
	}
	if st.Reserved {
		t.Fatal("阈值 0（关闭）时不应有 Reserved 标记")
	}

	p.SetReserveCredits(10)
	if st, _ = p.Status("poor"); !st.Reserved {
		t.Fatal("credits=5 <= reserve=10 时应有 Reserved 标记")
	}

	p.SetCredits("poor", 5, 0) // 余额未知
	if st, _ = p.Status("poor"); st.Reserved {
		t.Fatal("余额未知时不应有 Reserved 标记")
	}

	p.SetCredits("poor", 500, 1000) // 回血
	if st, _ = p.Status("poor"); st.Reserved {
		t.Fatal("余额充足时不应有 Reserved 标记")
	}
}
