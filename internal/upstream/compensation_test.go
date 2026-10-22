// compensation_test.go 官方福利状态查询（补偿 / 新手礼包）：形状、路径、Pending 判据。
package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestGetCompensationStatus 解析补偿状态；路径必须是 /billing/meter/compensation-status。
func TestGetCompensationStatus(t *testing.T) {
	var gotPath string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			return jsonResp(405, `{"code":1,"msg":"want POST: `+string(raw)+`"}`), nil
		}
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,
			"credit_num":500,"validity_period":1,"end_time":"2026-10-22 23:59:59"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}

	st, err := c.GetCompensationStatus(a)
	if err != nil {
		t.Fatalf("GetCompensationStatus: %v", err)
	}
	if gotPath != "/billing/meter/compensation-status" {
		t.Fatalf("路径=%s，want /billing/meter/compensation-status", gotPath)
	}
	if !st.Active || st.Claimed || st.CreditNum != 500 || st.EndTime != "2026-10-22 23:59:59" {
		t.Fatalf("状态=%+v", st)
	}
	if !st.Pending() {
		t.Fatal("active && !claimed && credit>0 应判为待领")
	}
}

// TestGetGiftStatusPath 礼包状态端点**无 /v2 前缀**（带 /v2 上游是 404）。
func TestGetGiftStatusPath(t *testing.T) {
	var gotPath string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		return jsonResp(200, `{"code":0,"data":{"claimed":true,"active":true,"credit_num":1500,
			"end_time":"2026-10-31 23:59:59"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}
	st, err := c.GetGiftStatus(a)
	if err != nil {
		t.Fatalf("GetGiftStatus: %v", err)
	}
	if gotPath != "/billing/meter/check-gift-claimed" {
		t.Fatalf("路径=%s，want /billing/meter/check-gift-claimed（无 /v2 前缀）", gotPath)
	}
	if !st.Claimed || st.Pending() {
		t.Fatalf("已领过的礼包不应判为待领：%+v", st)
	}
}

// TestCompensationPending 三与门：active / !claimed / credit>0 缺一即「无需领取」。
func TestCompensationPending(t *testing.T) {
	cases := []struct {
		name string
		st   CompensationStatus
		want bool
	}{
		{"可领", CompensationStatus{Active: true, Claimed: false, CreditNum: 500}, true},
		{"无活动", CompensationStatus{Active: false, CreditNum: 500}, false},
		{"已领", CompensationStatus{Active: true, Claimed: true, CreditNum: 500}, false},
		{"零额度", CompensationStatus{Active: true, Claimed: false, CreditNum: 0}, false},
		{"全空", CompensationStatus{}, false},
	}
	for _, c := range cases {
		if got := c.st.Pending(); got != c.want {
			t.Errorf("%s: Pending()=%v want %v", c.name, got, c.want)
		}
	}
}

// TestCompensationStatusNoActivityIsNotError 「无活动」是正常 200（active=false），
// 不得返回错误——否则没活动的号会把日志刷成一片错误。
func TestCompensationStatusNoActivityIsNotError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"active":false,"claimed":false,"credit_num":0}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}
	st, err := c.GetCompensationStatus(a)
	if err != nil {
		t.Fatalf("无活动不应是错误：%v", err)
	}
	if st.Active || st.Pending() {
		t.Fatalf("无活动状态=%+v", st)
	}
}

// TestCompensationStatusUpstreamError 非 2xx / 传输失败才报错（global 号打 CN billing 恒 401：
// 这条错误必须能浮出来，而不是被盲领静默吃掉）。
func TestCompensationStatusUpstreamError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(401, `{"code":401,"msg":"unauthorized"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}
	_, err := c.GetCompensationStatus(a)
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误应带状态码：%v", err)
	}
}
