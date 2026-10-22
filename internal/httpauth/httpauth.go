// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		// 缺头/方案不对：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// VerifyAPIKey 校验请求头是否携带正确的密钥，接受两种形态：
//
//	Authorization: Bearer <key>   （OpenAI 口径，也是本网关既有口径）
//	x-api-key: <key>              （Anthropic 口径，Claude Code 用这个）
//
// key 为空表示"未启用鉴权"，恒返回 true（与 VerifyBearer 同语义）。
// 两种形态都走 SHA-256 摘要 + 常量时间比较；两条路径都执行再取或（不做短路），
// 避免「带哪种头」在耗时上形成可观测差异。注意：这只抹平比较阶段，网络与
// 上层处理耗时仍占绝对多数，不宣称端到端恒定时间。
func VerifyAPIKey(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	authzOK := false
	if strings.HasPrefix(authz, bearerPrefix) {
		authzOK = subtle.ConstantTimeCompare(digest(authz[len(bearerPrefix):]), digest(key)) == 1
	} else {
		_ = subtle.ConstantTimeCompare(digest(""), digest(key))
	}
	apiKeyOK := subtle.ConstantTimeCompare(digest(r.Header.Get("x-api-key")), digest(key)) == 1
	return authzOK || apiKeyOK
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
