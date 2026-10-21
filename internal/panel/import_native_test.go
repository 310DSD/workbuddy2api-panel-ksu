package panel

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// nativeAuthJSON 生成一个原生 auth 格式的账号对象（嵌套形，与 auths/*.json 一致）。
func nativeAuthJSON(uid, at, rt string) string {
	return `{"auth":{"accessToken":"` + at + `","refreshToken":"` + rt + `","expiresAt":4102444800,"domain":"www.codebuddy.cn","realm":"cn"},"account":{"uid":"` + uid + `","enterpriseId":"","nickname":"nick-` + uid + `"}}`
}

func newImportTestPanel(t *testing.T, authDir string) *Panel {
	t.Helper()
	return New(Config{
		Version: "test",
		APIKey:  "k",
		AuthDir: authDir,
		Pool:    pool.New(filepath.Join(authDir, "state.json")),
	})
}

// postNativeImport 构造 multipart 请求并直接调用 handler（绕过 mux，专注导入逻辑）。
func postNativeImport(t *testing.T, p *Panel, body string, overwrite string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if overwrite != "" {
		_ = mw.WriteField("overwrite", overwrite)
	}
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/panel/api/import/native", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	p.importNative(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// 单文件（一个账号对象）→ 落盘为 workbuddy-<uid>.json，且能被 auth.LoadDir 读回。
func TestImportNativeSingleObject(t *testing.T) {
	dir := t.TempDir()
	p := newImportTestPanel(t, dir)
	code, out := postNativeImport(t, p, nativeAuthJSON("u-1", "at1", "rt1"), "")
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["imported"] != float64(1) {
		t.Fatalf("imported=%v want 1 (%v)", out["imported"], out)
	}
	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || auths[0].UID != "u-1" || auths[0].AccessToken != "at1" {
		t.Fatalf("roundtrip mismatch: %+v", auths)
	}
	if p.cfg.Pool.AuthByUID("u-1") == nil {
		t.Fatal("account not hot-loaded into pool")
	}
}

// 数组形态批量导入 + 扁平形兼容。
func TestImportNativeArrayAndFlatShape(t *testing.T) {
	dir := t.TempDir()
	p := newImportTestPanel(t, dir)
	flat := `{"accessToken":"at-flat","refreshToken":"rt-flat","expiresAt":4102444800,"uid":"u-flat","nickname":"n"}`
	body := "[" + nativeAuthJSON("u-a", "at-a", "rt-a") + "," + nativeAuthJSON("u-b", "at-b", "rt-b") + "," + flat + "]"
	code, out := postNativeImport(t, p, body, "")
	if code != http.StatusOK || out["imported"] != float64(3) {
		t.Fatalf("code=%d out=%v", code, out)
	}
	for _, uid := range []string{"u-a", "u-b", "u-flat"} {
		if p.cfg.Pool.AuthByUID(uid) == nil {
			t.Errorf("uid %s not in pool", uid)
		}
	}
}

// 默认不覆盖已存在账号；overwrite=1 才替换（并清掉旧状态）。
func TestImportNativeOverwritePolicy(t *testing.T) {
	dir := t.TempDir()
	p := newImportTestPanel(t, dir)
	if code, out := postNativeImport(t, p, nativeAuthJSON("u-1", "old", "rt-old"), ""); code != 200 || out["imported"] != float64(1) {
		t.Fatalf("first import: code=%d out=%v", code, out)
	}
	// 默认：跳过
	code, out := postNativeImport(t, p, nativeAuthJSON("u-1", "new", "rt-new"), "")
	if code != 200 || out["imported"] != float64(0) || out["skipped"] != float64(1) {
		t.Fatalf("default should skip existing: code=%d out=%v", code, out)
	}
	if got := p.cfg.Pool.AuthByUID("u-1").AccessTokenValue(); got != "old" {
		t.Fatalf("existing token must not be overwritten, got %q", got)
	}
	// 显式覆盖
	code, out = postNativeImport(t, p, nativeAuthJSON("u-1", "new", "rt-new"), "1")
	if code != 200 || out["imported"] != float64(1) || out["overwritten"] != float64(1) {
		t.Fatalf("overwrite: code=%d out=%v", code, out)
	}
	if got := p.cfg.Pool.AuthByUID("u-1").AccessTokenValue(); got != "new" {
		t.Fatalf("overwrite token = %q want new", got)
	}
}

// 非法 uid 必须拒绝（防路径穿越），且不落盘。
func TestImportNativeRejectsBadUID(t *testing.T) {
	dir := t.TempDir()
	p := newImportTestPanel(t, dir)
	for _, uid := range []string{"../../evil", "a/b", "uid with space"} {
		body := strings.Replace(nativeAuthJSON("PLACEHOLDER", "at", "rt"), "PLACEHOLDER", uid, 1)
		code, out := postNativeImport(t, p, body, "")
		if code != http.StatusOK {
			t.Fatalf("uid %q: code=%d", uid, code)
		}
		if out["imported"] != float64(0) || out["skipped"] != float64(1) {
			t.Fatalf("uid %q should be skipped: %v", uid, out)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "workbuddy-") {
			t.Fatalf("bad uid wrote file: %s", e.Name())
		}
	}
}

// {"accounts":[...]} 信封形态。
func TestImportNativeAccountsEnvelope(t *testing.T) {
	dir := t.TempDir()
	p := newImportTestPanel(t, dir)
	body := `{"accounts":[` + nativeAuthJSON("u-e", "at", "rt") + `]}`
	code, out := postNativeImport(t, p, body, "")
	if code != 200 || out["imported"] != float64(1) {
		t.Fatalf("envelope: code=%d out=%v", code, out)
	}
	if p.cfg.Pool.AuthByUID("u-e") == nil {
		t.Fatal("envelope account not imported")
	}
}
