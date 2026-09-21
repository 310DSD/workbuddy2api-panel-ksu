#!/system/bin/sh
# WorkBuddy2API Panel · KernelSU 安装（仅 arm64-v8a）
SKIPUNZIP=0
if ! type ui_print >/dev/null 2>&1; then ui_print() { echo "$1"; }; fi
if ! type abort >/dev/null 2>&1; then abort() { ui_print "$1"; exit 1; }; fi

ui_print "******************************"
ui_print " WorkBuddy2API Panel"
ui_print " arm64-v8a · v1.11.1 · Go 1.27"
ui_print "******************************"

ABI=$(getprop ro.product.cpu.abi)
ui_print "- ABI: $ABI"
case "$ABI" in
  arm64-v8a) ;;
  *) abort "! 本包仅支持 arm64-v8a，当前: $ABI" ;;
esac

# Android unzip 通常不保留 zip 里的 Unix 执行位，不能用 -x 判断。
if [ ! -f "$MODPATH/bin/wb2api" ]; then
  abort "! 缺少 bin/wb2api"
fi
chmod 755 "$MODPATH/bin/wb2api"

DATADIR=/data/adb/wb2api
mkdir -p "$DATADIR/auths" "$DATADIR/data" "$DATADIR/logs"
chmod 700 "$DATADIR" "$DATADIR/auths" "$DATADIR/data"

chmod 755 "$MODPATH/bin/wb2api" \
          "$MODPATH/scripts/control.sh" \
          "$MODPATH/service.sh" \
          "$MODPATH/action.sh" \
          "$MODPATH/uninstall.sh" 2>/dev/null
chmod 644 "$MODPATH/module.prop" "$MODPATH/webroot/"* 2>/dev/null
touch "$MODPATH/skip_mount"

if [ ! -f "$DATADIR/config.json" ]; then
  KEY="sk-$(tr -dc 'a-zA-Z0-9' </dev/urandom 2>/dev/null | dd bs=1 count=24 2>/dev/null)"
  [ ${#KEY} -lt 16 ] && KEY="sk-$(cat /proc/sys/kernel/random/uuid | tr -d '-')"
  cat > "$DATADIR/config.json" <<EOF
{
  "listen": "0.0.0.0:7863",
  "api_key": "$KEY",
  "auth_dir": "/data/adb/wb2api/auths",
  "state_file": "/data/adb/wb2api/data/state.json",
  "cooldown": { "soft_rate": "600s", "soft_rate_max": "2h" },
  "schedule": {
    "checkin_hours": [9, 21],
    "travel_hours": [9, 21],
    "activity_hours": [10],
    "keepalive_hours": [22],
    "blackcat_hours": [23],
    "checkin_enabled": true,
    "travel_enabled": true,
    "activity_enabled": true,
    "keepalive_enabled": true,
    "blackcat_enabled": true,
    "balance_refresh_enabled": true,
    "balance_refresh_minutes": 5
  },
  "global": { "enabled": true, "chat_base": "", "billing_base": "" },
  "upstream": {
    "timeout_seconds": 120,
    "header_timeout_seconds": 120,
    "idle_timeout_seconds": 300,
    "user_agent": "",
    "client_version": "",
    "cli_version": "",
    "client_name": "",
    "device_token": "",
    "device_token_file": "",
    "passthrough_ip": false
  },
  "features": { "sanitize_blacklist_fingerprints": true },
  "prompt": { "mode": "passthrough", "file": "" },
  "upstash": { "url": "", "token": "" },
  "pool": {
    "max_in_flight": 3,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m",
    "breaker_cooldown_max": "6h",
    "idle_weight_per_hour": 0.5,
    "idle_weight_max": 5.0,
    "expiring_soon": "168h"
  },
  "session_sticky": { "enabled": true, "ttl": "30m", "gc_interval": "5m" }
}
EOF
  chmod 600 "$DATADIR/config.json"
  printf '%s\n' "$KEY" > "$DATADIR/api_key.txt"
  chmod 600 "$DATADIR/api_key.txt"
  ui_print "- 已生成配置: $DATADIR/config.json"
  ui_print "- API Key: $KEY"
else
  ui_print "- 保留已有配置 $DATADIR/config.json"
fi

if type set_perm_recursive >/dev/null 2>&1; then
  set_perm_recursive "$MODPATH" 0 0 0755 0644
  set_perm "$MODPATH/bin/wb2api" 0 0 0755
  set_perm "$MODPATH/scripts/control.sh" 0 0 0755
  set_perm "$MODPATH/service.sh" 0 0 0755
  set_perm "$MODPATH/action.sh" 0 0 0755
  set_perm "$MODPATH/uninstall.sh" 0 0 0755
fi

ui_print "- 面板: http://127.0.0.1:7863/panel/"
ui_print "- 打开模块 WebUI 即进入管理面板"
ui_print "- 局域网可用 http://<手机IP>:7863"