# WorkBuddy2API Panel · KernelSU 移植

本仓库是 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) 的 **GitHub 官方 Fork**，基于上游 **v1.13.0**（commit 6b31db6，2026-10-12；v1.10.0 = 4f18f7f 起同步），叠加 Android 手机端改动。

授权使用边界与上游相同：仅限本人授权账号、本机 / 私有环境测试；不得共享、转售或用于违反目标平台条款的用途。KernelSU 模块不要把 `7863` 暴露到公网。

## 内容

- 根目录：Go 网关源码（`cmd/` `internal/` `go.mod` 等）
- `kernelsu/`：KernelSU 模块工程（不含预编译二进制）
- 版本：`1.13.0-ksu`，Go 1.27.2 交叉编译目标 android/arm64

## 本 fork 相对上游的增量

- `cmd/server/dns_android.go`：Android 静态 Go 绕过 `[::1]:53`
- `internal/panel/index.html` / `app.js`：竖屏底栏 + 账号卡片
- `internal/panel/ring.go`：日志环不泄漏 backing array
- `internal/pool/persist.go`：state.json 紧凑落盘
- 依赖升到最新稳定版（redis v9.23.0、x/sys v0.49.0、atomic v1.12.0）
- WebUI / 开机脚本统一走 `ksud module action wb2api_panel`，避免宿主 App freezer

## 构建模块二进制

```sh
# GOTOOLCHAIN=auto：本地 Go 低于 go.mod 要求时自动下载对应工具链
# （proxy.golang.org 不可达时设 GOPROXY=https://goproxy.cn,direct 兜底）
export CGO_ENABLED=0 GOOS=android GOARCH=arm64 GOTOOLCHAIN=auto
go build -trimpath -ldflags='-s -w -buildid=' -o kernelsu/bin/wb2api ./cmd/server
```
