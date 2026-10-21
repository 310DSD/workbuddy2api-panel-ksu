package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	TokenType      string `json:"token_type"`
	ExpiresAt      int64  `json:"expires_at"`
	Domain         string `json:"domain"`
	DosageNotify   string `json:"dosage_notify_code"`
	PaymentType    string `json:"payment_type"`
	Status         string `json:"status"`
	UsageUpdatedAt int64  `json:"usage_updated_at"`
	LastCheckin    int64  `json:"last_checkin_time"`
	CheckinStreak  int    `json:"checkin_streak"`
	CreatedAt      int64  `json:"created_at"`
	LastUsed       int64  `json:"last_used"`
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, panelMaxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	var accounts []cockpitAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var total, imported, skipped int
	var errs []string

	for _, acc := range accounts {
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			skipped++
			errs = append(errs, fmt.Sprintf("missing required fields (id=%s)", acc.ID))
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("invalid uid (id=%s)", acc.ID))
			continue
		}

		// 按 domain 推断 realm：workbuddy.ai 家族 → global，否则 cn。
		realm := auth.ResolveRealm("", acc.Domain)

		// cockpit tools 的 expires_at 为毫秒时间戳，转为秒。
		expiresAt := acc.ExpiresAt / 1000
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}

		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			Nickname:     nickname,
			FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
		}

		if realm == "global" {
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else {
			_, _ = a.BackfillRealm()
		}

		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
			continue
		}

		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(uid)

		// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
		if realm == "global" {
			if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
				log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
			} else if activated {
				log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
			}
			if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
				log.Printf("panel: import global trial uid=%s: %v", uid, err)
			} else if claimed {
				log.Printf("panel: import global trial uid=%s 已领", uid)
			}
		} else {
			if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
				log.Printf("panel: import checkin uid=%s: %v", uid, err)
			}
		}
		if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
			p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
		}

		imported++
	}

	total = len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// importNative 导入「原生 auth 格式」账号：单文件（一个账号）或数组（多账号）均可，
// 也兼容 {"accounts":[...]} 信封。字段与 auths/*.json 完全一致：
//
//	嵌套形 {"auth":{"accessToken","refreshToken","expiresAt","domain","realm"},"account":{"uid","enterpriseId","nickname"},"device_token"}
//	扁平形 {"accessToken","refreshToken","expiresAt","domain","realm","uid","enterpriseId","nickname","device_token"}
//
// 与 cockpit 导入的差异（刻意）：
//   - 不做签到 / 注册激活 / 余额拉取等上游副作用——纯「把凭证放进池」；
//     余额与状态交给既有后台余额刷新与签到时点。
//   - 默认**不覆盖**已存在的 uid（避免用陈旧 token 冲掉线上正在刷新的凭证）；
//     overwrite=1 才覆盖。
//
// POST /panel/api/import/native?overwrite=0|1  multipart: file=<json>
func (p *Panel) importNative(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, panelMaxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}
	overwrite := false
	switch strings.ToLower(strings.TrimSpace(r.FormValue("overwrite"))) {
	case "1", "true", "yes", "on":
		overwrite = true
	}

	docs, err := splitNativeDocs(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(docs) == 0 {
		writeErr(w, http.StatusBadRequest, "empty: 文件里没有可导入的账号对象")
		return
	}
	if p.cfg.AuthDir == "" {
		writeErr(w, http.StatusInternalServerError, "auth_dir 未配置")
		return
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}

	var imported, skipped, overwritten int
	var errs []string
	seen := make(map[string]bool, len(docs))

	for i, doc := range docs {
		a, err := auth.Parse(doc)
		if err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("第 %d 项: %v", i+1, err))
			continue
		}
		uid := strings.TrimSpace(a.UID)
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("第 %d 项: uid 非法或缺失（%q）", i+1, uid))
			continue
		}
		if seen[uid] {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: 文件内重复，已跳过", uid))
			continue
		}
		seen[uid] = true
		exists := p.cfg.Pool.AuthByUID(uid) != nil
		if exists && !overwrite {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: 账号已存在（勾选覆盖可替换）", uid))
			continue
		}

		// 与登录落盘同口径：显式写 realm（cn/global），让文件形态统一。
		if a.RealmStored() == "" {
			if _, rerr := auth.BackfillRealmFor(a, a.Realm()); rerr != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, rerr))
				continue
			}
		}
		a.FilePath = filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid))
		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
			continue
		}

		// 已存在且允许覆盖：先把旧对象摘掉（含冷却/熔断状态），再以新凭证入池，
		// 避免 upsert 保留旧状态后仍按旧冷却时间拒流。overwrite 是显式运维动作。
		if exists {
			p.cfg.Pool.Remove(uid)
			overwritten++
		}
		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(uid)
		imported++
	}

	log.Printf("panel: native import finished total=%d imported=%d overwritten=%d skipped=%d",
		len(docs), imported, overwritten, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"total":       len(docs),
		"imported":    imported,
		"overwritten": overwritten,
		"skipped":     skipped,
		"errors":      errs,
	})
}

// splitNativeDocs 把上传内容拆成「单个账号对象」列表，兼容三种形态：
//
//	单对象   {...}                  （标准 auths/workbuddy-<uid>.json 内容）
//	数组     [{...},{...}]
//	信封     {"accounts":[{...}]}
func splitNativeDocs(raw []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty file")
	}
	switch trimmed[0] {
	case '{':
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &probe); err != nil {
			return nil, fmt.Errorf("invalid json: %w", err)
		}
		// 信封形态：{"accounts":[...]}（state.json 风格的批量导出）。
		if acc, ok := probe["accounts"]; ok {
			var arr []json.RawMessage
			if err := json.Unmarshal(acc, &arr); err != nil {
				return nil, fmt.Errorf("invalid accounts array: %w", err)
			}
			return arr, nil
		}
		return []json.RawMessage{json.RawMessage(trimmed)}, nil
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("invalid json array: %w", err)
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("invalid json: 期望对象或数组")
	}
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
