# WorkBuddy2API Panel · KernelSU（arm64-v8a）

开机自启网关。KernelSU **WebUI** 只做入口（启停走模块 Action / `ksud`，与 Action 按钮同一通道），点按钮用系统浏览器打开 `http://127.0.0.1:7863/panel/`。

默认监听 `0.0.0.0:7863`。同一 Wi-Fi 下电脑可用 `http://<手机IP>:7863`。

| 路径 | 说明 |
|---|---|
| `/data/adb/wb2api/config.json` | 配置（升级不覆盖） |
| `/data/adb/wb2api/auths/` | 账号凭证 |
| `/data/adb/wb2api/api_key.txt` | API Key（权限 600） |
| `http://127.0.0.1:7863/v1` | OpenAI 兼容 API |

卸载默认保留账号。若要连数据删除：`touch /data/adb/wb2api/.wipe_on_uninstall`