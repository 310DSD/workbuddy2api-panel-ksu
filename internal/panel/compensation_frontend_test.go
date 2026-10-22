package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 官方补偿卡片的渲染契约：
//   - 只有「可领且未领」给领取按钮；
//   - 查询失败必须显形（不能显示成「无补偿」）；
//   - 截止 ≤48h 标红、已过期也说清楚。
func TestAppJSCompensationRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; compensation render test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function compStatusText');
const end = src.indexOf('async function loadCompensation');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const durStart = src.indexOf('function dur(');
const durEnd = src.indexOf('function parseAPITime');
if ([start, end, escStart, escEnd, fmtStart, fmtEnd, durStart, durEnd].some(v => v < 0)) throw new Error('compensation helpers not found');
const host = { innerHTML: '', textContent: '' };
const ctx = Object.assign({}, {
  Date, Number, String, Math, Array, Object, isNaN, RegExp,
  document: { getElementById: () => host },
  $: () => host,
});
vm.createContext(ctx);
vm.runInContext(src.slice(durStart, durEnd) + src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(start, end) +
  '\nthis.compStatusText=compStatusText; this.renderCompensation=renderCompensation;', ctx);

const txt = r => ctx.compStatusText(r).text;
ctx.renderCompensation({ pending: 2, failed: 1, accounts: [
  { uid: 'u1', nickname: '一号', active: true, claimed: false, credit_num: 500, end_time: '2026-10-22 23:59:59', seconds_left: 108000, pending: true },
  { uid: 'u2', nickname: '二号', active: true, claimed: true, credit_num: 500, seconds_left: 0, pending: false },
  { uid: 'u3', nickname: '三号', error: 'status 401', seconds_left: 0, pending: false },
  { uid: 'u4', active: true, claimed: false, credit_num: 0, seconds_left: 0, pending: false },
  { uid: 'u5', active: false, claimed: false, credit_num: 0, seconds_left: 0, pending: false },
]});
const html = host.innerHTML;
process.stdout.write(JSON.stringify({
  pendingBtn: (html.match(/data-comp=/g) || []).length,
  claimText: txt({ active: true, claimed: false, credit_num: 500, end_time: '2026-10-22 23:59:59', seconds_left: 108000 }).includes('可领'),
  soonRed: ctx.compStatusText({ active: true, claimed: false, credit_num: 1, seconds_left: 108000 }).cls === 'var(--bad)',
  farAmber: ctx.compStatusText({ active: true, claimed: false, credit_num: 1, seconds_left: 720000 }).cls === 'var(--warn)',
  expired: txt({ active: true, claimed: false, credit_num: 1, seconds_left: 0, expired: true }).includes('已过期'),
  claimed: txt({ active: true, claimed: true, credit_num: 500 }).includes('已领取'),
  noActivity: txt({ active: false }).includes('无补偿活动'),
  errorShown: html.includes('查询失败'),
  note: host.textContent,
}));`
	f, err := os.CreateTemp(t.TempDir(), "compensation-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("compensation render node test failed: %v\n%s", err, out)
	}
	var got struct {
		PendingBtn int    `json:"pendingBtn"`
		ClaimText  bool   `json:"claimText"`
		SoonRed    bool   `json:"soonRed"`
		FarAmber   bool   `json:"farAmber"`
		Expired    bool   `json:"expired"`
		Claimed    bool   `json:"claimed"`
		NoActivity bool   `json:"noActivity"`
		ErrorShown bool   `json:"errorShown"`
		Note       string `json:"note"`
	}
	outStr := strings.TrimSpace(string(out))
	if err := json.Unmarshal([]byte(outStr), &got); err != nil {
		t.Fatalf("输出解析失败: %v (%s)", err, outStr)
	}
	if !got.ClaimText {
		t.Errorf("可领文案缺失：%q", outStr)
	}
	if got.PendingBtn != 1 {
		t.Errorf("领取按钮数=%d want 1（只有可领未领的号给按钮）", got.PendingBtn)
	}
	if !got.SoonRed || !got.FarAmber {
		t.Errorf("截止色标：≤48h 红=%v、远期琥珀=%v，want true/true", got.SoonRed, got.FarAmber)
	}
	if !got.Expired || !got.Claimed || !got.NoActivity {
		t.Errorf("状态文案：过期=%v 已领=%v 无活动=%v（want 全 true）", got.Expired, got.Claimed, got.NoActivity)
	}
	if !got.ErrorShown {
		t.Error("查询失败必须在卡片上显形")
	}
	if !strings.Contains(got.Note, "2 个账号有待领补偿") || !strings.Contains(got.Note, "1 个查询失败") {
		t.Errorf("提示栏=%q，want 含待领 2 / 失败 1", got.Note)
	}
}
