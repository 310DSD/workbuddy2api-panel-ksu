// modelbusy_test.go 上游 /v2/chat/busy 繁忙度观测：只读、分批、按需不填。
package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelBusyParsesLevels 基本形状：POST /v2/chat/busy + 信封 data 数组 → model→level；
// 上游未返回的模型不在 map 里（调用方按「未知」处理，不得当成不可用）。
func TestModelBusyParsesLevels(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath, gotMethod = r.URL.Path, r.Method
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		return jsonResp(200, `{"code":0,"data":[
			{"model":"glm-5.3-flash","busy_level":"idle"},
			{"model":"deepseek-v4-pro","busy_level":"congested"},
			{"model":"unknown-only"},
			{"model":"","busy_level":"busy"}
		]}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}

	got, err := c.ModelBusy(a, []string{"glm-5.3-flash", "deepseek-v4-pro", "no-data"})
	if err != nil {
		t.Fatalf("ModelBusy: %v", err)
	}
	if gotPath != "/v2/chat/busy" || gotMethod != http.MethodPost {
		t.Fatalf("请求=%s %s，want POST /v2/chat/busy", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"models":["glm-5.3-flash","deepseek-v4-pro","no-data"]`) {
		t.Fatalf("请求体=%s", gotBody)
	}
	if len(got) != 2 || got["glm-5.3-flash"] != "idle" || got["deepseek-v4-pro"] != "congested" {
		t.Fatalf("解析结果=%v", got)
	}
	if _, ok := got["no-data"]; ok {
		t.Fatal("上游未返回的模型不应出现在 map 里")
	}
	if _, ok := got[""]; ok {
		t.Fatal("空模型名不应入 map")
	}
}

// TestModelBusyBatches50 单批上限 50：51 个模型必须拆成 2 次请求（超限整批被上游拒）。
func TestModelBusyBatches50(t *testing.T) {
	calls := 0
	sizes := []int{}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		var req struct {
			Models []string `json:"models"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		sizes = append(sizes, len(req.Models))
		rows := make([]string, 0, len(req.Models))
		for _, m := range req.Models {
			rows = append(rows, `{"model":"`+m+`","busy_level":"normal"}`)
		}
		return jsonResp(200, `{"code":0,"data":[`+strings.Join(rows, ",")+`]}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}

	models := make([]string, 51)
	for i := range models {
		models[i] = "m" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	got, err := c.ModelBusy(a, models)
	if err != nil {
		t.Fatalf("ModelBusy: %v", err)
	}
	if calls != 2 {
		t.Fatalf("请求次数=%d，want 2（51 个模型分两批）", calls)
	}
	if len(sizes) != 2 || sizes[0] != 50 || sizes[1] != 1 {
		t.Fatalf("分批大小=%v，want [50 1]", sizes)
	}
	if len(got) != 51 {
		t.Fatalf("解析到 %d 个模型，want 51", len(got))
	}
}

// TestModelBusyUpstreamError 上游错误原样返回（调用方不填即可，不得影响 models 端点）。
func TestModelBusyUpstreamError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(500, `{"code":500,"msg":"internal"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}
	if _, err := c.ModelBusy(a, []string{"glm-5.3-flash"}); err == nil {
		t.Fatal("上游 500 时应返回错误")
	}

	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})
	if _, err := c2.ModelBusy(a, []string{"glm-5.3-flash"}); err == nil {
		t.Fatal("传输层失败时应返回错误")
	}
}

// TestBusyWarning 只有 busy/congested/unavailable 值得挂色标：
// idle/normal 是常态，unknown 是「当次没查到」而非不可用，都不能告警。
func TestBusyWarning(t *testing.T) {
	warn := map[string]bool{"busy": true, "congested": true, "unavailable": true}
	for level, want := range warn {
		if got := BusyWarning(level); got != want {
			t.Errorf("BusyWarning(%q)=%v want %v", level, got, want)
		}
	}
	for _, level := range []string{"idle", "normal", "unknown", ""} {
		if BusyWarning(level) {
			t.Errorf("BusyWarning(%q)=true，want false（不告警）", level)
		}
	}
}
