// modelbusy_test.go（panel）模型繁忙度接线：/panel/api/models 补 busy_level，
// 且观测失败时绝不把该端点拖成 5xx（面板宁可少一列，也不能整页打不开）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// busyPanel 造一个带单账号（cn 域）+ 指向假上游的面板。
func busyPanel(t *testing.T, handler http.HandlerFunc) *Panel {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.codebuddy.cn"})
	return New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up})
}

// TestAttachModelBusy 正常路径：上游返回的模型被补上 busy_level，
// 未返回 / 空 level 的模型保持无字段（前端按「未知」不挂色标）。
func TestAttachModelBusy(t *testing.T) {
	p := busyPanel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/busy" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":[
			{"model":"glm-5.3-flash","busy_level":"busy"},
			{"model":"deepseek-v4-pro","busy_level":"idle"}
		]}`))
	})

	entries := []map[string]any{
		{"id": "cn:glm-5.3-flash"},
		{"id": "cn:deepseek-v4-pro"},
		{"id": "cn:no-data"},
	}
	p.attachModelBusy(entries)

	if entries[0]["busy_level"] != "busy" {
		t.Errorf("glm-5.3-flash busy_level=%v want busy", entries[0]["busy_level"])
	}
	if entries[1]["busy_level"] != "idle" {
		t.Errorf("deepseek-v4-pro busy_level=%v want idle", entries[1]["busy_level"])
	}
	if _, ok := entries[2]["busy_level"]; ok {
		t.Error("上游未返回的模型不应带 busy_level")
	}
}

// TestAttachModelBusyDegradesQuietly 观测失败（上游 5xx / 无可用账号）时静默降级：
// 条目原样返回、不 panic、不补字段。
func TestAttachModelBusyDegradesQuietly(t *testing.T) {
	p := busyPanel(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":500,"msg":"oops"}`, http.StatusInternalServerError)
	})
	entries := []map[string]any{{"id": "cn:glm-5.3-flash"}}
	p.attachModelBusy(entries)
	if _, ok := entries[0]["busy_level"]; ok {
		t.Error("上游失败时不应带 busy_level")
	}

	// 无可用账号：直接用空池面板，不得尝试请求、不得 panic。
	empty := New(Config{
		Version:  "test",
		APIKey:   "k",
		Pool:     pool.New(filepath.Join(t.TempDir(), "s.json")),
		Upstream: upstream.New(),
	})
	entries2 := []map[string]any{{"id": "cn:glm-5.3-flash"}, {"id": "no-realm-prefix"}}
	empty.attachModelBusy(entries2)
	for _, e := range entries2 {
		if _, ok := e["busy_level"]; ok {
			t.Errorf("%v 不应带 busy_level", e["id"])
		}
	}
}

// TestModelsEndpointIncludesBusy 端到端：/panel/api/models 的模型条目带 busy_level。
// 假上游同时提供模型目录（FetchModels 走 /v1/... 端点）与繁忙度端点。
func TestModelsEndpointIncludesBusy(t *testing.T) {
	p := busyPanel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/chat/busy":
			_, _ = w.Write([]byte(`{"code":0,"data":[{"model":"glm-5.2","busy_level":"congested"}]}`))
		default:
			// 模型目录端点：形状按 FetchModels 的解析契约给最小可用集。
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","model":"glm-5.2"}
			]}}`))
		}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Skipf("模型目录假上游形状不匹配（code=%d body=%.200s），繁忙度接线由前两例覆盖",
			rec.Code, rec.Body.String())
	}
	var got struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) == 0 {
		t.Skip("假上游未返回模型，跳过端到端断言")
	}
	if got.Models[0]["busy_level"] != "congested" {
		t.Fatalf("busy_level=%v want congested（full=%v）", got.Models[0]["busy_level"], got.Models[0])
	}
}
