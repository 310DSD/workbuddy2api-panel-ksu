package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 模型行的繁忙度色标：只有 busy / congested / unavailable 挂标签，
// idle / normal（常态）与 unknown（当次没查到）都不挂——否则满屏噪音，
// 且「查不到」会被误读成「不可用」。
func TestAppJSModelBusyBadge(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model busy badge test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function fmtK');
const end = src.indexOf('function renderModels');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
if (start < 0 || end < 0 || end < start || escStart < 0 || escEnd < 0) throw new Error('model row helpers not found');
const ctx = { Date, Number, String, Array, Object, Math, isNaN };
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(start, end) + '\nthis.mdRowHtml=mdRowHtml;', ctx);
const base = { id: 'cn:glm-5.3-flash', name: 'GLM-5.3-Flash', context_length: 200000 };
const row = extra => ctx.mdRowHtml(Object.assign({}, base, extra), null);
const has = (html, s) => html.includes(s);
process.stdout.write(JSON.stringify({
  busy: has(row({ busy_level: 'busy' }), '繁忙'),
  congested: has(row({ busy_level: 'congested' }), '拥堵'),
  unavailable: has(row({ busy_level: 'unavailable' }), '不可用'),
  idle: has(row({ busy_level: 'idle' }), 'tag bad'),
  normal: has(row({ busy_level: 'normal' }), 'tag bad'),
  unknown: has(row({ busy_level: 'unknown' }), 'tag bad'),
  absent: has(row({}), 'tag bad'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-busy-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model busy badge node test failed: %v\n%s", err, out)
	}
	const want = `{"busy":true,"congested":true,"unavailable":true,"idle":false,"normal":false,"unknown":false,"absent":false}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("busy badge=%s\nwant %s", out, want)
	}
}
