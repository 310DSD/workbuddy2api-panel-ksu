// reserve_credits_config_test.go pool.reserve_credits 配置测试：
// 默认 0（关闭，零回归）/ 文件覆盖 / 负值钳 0 / 与 credit_floor 的共存关系。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReserveCreditsDefault 键缺席 → 默认 0（保留积分关闭，行为与引入前一致）。
func TestReserveCreditsDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.ReserveCredits != 0 {
		t.Errorf("reserve_credits=%d want 0 (default off)", c.Pool.ReserveCredits)
	}
}

// TestReserveCreditsParsedFromFile 显式配置覆盖默认。
func TestReserveCreditsParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"reserve_credits":10}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.ReserveCredits != 10 {
		t.Errorf("reserve_credits=%d want 10", c.Pool.ReserveCredits)
	}
}

// TestReserveCreditsNegativeClamped 负值钳 0（非法即关闭，不报错：老配置误写不炸启动）。
func TestReserveCreditsNegativeClamped(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"reserve_credits":-5}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.ReserveCredits != 0 {
		t.Errorf("reserve_credits=%d want 0 (negative clamped)", c.Pool.ReserveCredits)
	}
}

// TestReserveCreditsCoexistsWithFloor reserve 与 floor 同时配置互不改写：
// 两档语义不同（floor 按模型收费性精细化，reserve 是账号级保命线），
// 反序配置（reserve > floor）只打 WARN、不钳值。
func TestReserveCreditsCoexistsWithFloor(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"credit_floor":100,"reserve_credits":500}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.CreditFloor != 100 {
		t.Errorf("credit_floor=%d want 100（不应被 reserve 改写）", c.Pool.CreditFloor)
	}
	if c.Pool.ReserveCredits != 500 {
		t.Errorf("reserve_credits=%d want 500（反序配置只告警，不钳值）", c.Pool.ReserveCredits)
	}
}
