#!/system/bin/sh
# WorkBuddy2API 启停控制（arm64）
MODDIR="${MODDIR:-}"
if [ -z "$MODDIR" ]; then
  HERE=$0
  case "$HERE" in
    /*) ;;
    *) HERE=$(command -v "$0" 2>/dev/null || echo "$0") ;;
  esac
  HERE=$(readlink -f "$HERE" 2>/dev/null || echo "$HERE")
  MODDIR=$(dirname "$(dirname "$HERE")")
fi

DATADIR=/data/adb/wb2api
BIN="$MODDIR/bin/wb2api"
CFG="$DATADIR/config.json"
LOG="$DATADIR/logs/wb2api.log"
PIDFILE="$DATADIR/wb2api.pid"

mkdir -p "$DATADIR/auths" "$DATADIR/data" "$DATADIR/logs"

busybox_bin() {
  for b in /data/adb/ksu/bin/busybox /data/adb/magisk/busybox /data/adb/ap/bin/busybox; do
    [ -x "$b" ] && { echo "$b"; return 0; }
  done
  return 1
}

listen_addr() {
  if [ -f "$CFG" ]; then
    sed -n 's/.*"listen"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CFG" | head -n 1
  fi
}

listen_port() {
  a=$(listen_addr)
  a=${a##*:}
  [ -n "$a" ] || a=7863
  echo "$a"
}

health_ok() {
  port=$(listen_port)
  url="http://127.0.0.1:${port}/healthz"
  BB=$(busybox_bin)
  if [ -n "$BB" ]; then
    out=$("$BB" wget -qO- -T 2 "$url" 2>/dev/null) || return 1
    echo "$out" | grep -q 'workbuddy2api' && return 0
    echo "$out" | grep -q 'healthy' && return 0
    return 1
  fi
  if command -v curl >/dev/null 2>&1; then
    out=$(curl -fsS --max-time 2 "$url" 2>/dev/null) || return 1
    echo "$out" | grep -q 'workbuddy2api' && return 0
    echo "$out" | grep -q 'healthy' && return 0
  fi
  return 1
}

is_our_pid() {
  pid=$1
  [ -n "$pid" ] && [ -d "/proc/$pid" ] || return 1
  comm=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null)
  case "$comm" in
    *wb2api*) return 0 ;;
  esac
  return 1
}

running_pid() {
  if [ -f "$PIDFILE" ]; then
    pid=$(cat "$PIDFILE" 2>/dev/null)
    if is_our_pid "$pid"; then
      echo "$pid"
      return 0
    fi
    rm -f "$PIDFILE"
  fi
  if command -v pidof >/dev/null 2>&1; then
    for pid in $(pidof wb2api 2>/dev/null); do
      if is_our_pid "$pid"; then
        echo "$pid" > "$PIDFILE"
        echo "$pid"
        return 0
      fi
    done
  fi
  return 1
}

rotate_log() {
  [ -f "$LOG" ] || return 0
  sz=$(wc -c < "$LOG" 2>/dev/null || echo 0)
  [ "$sz" -gt 5242880 ] || return 0
  mv -f "$LOG" "$LOG.old" 2>/dev/null
}

apply_env() {
  export TZ="${TZ:-Asia/Shanghai}"
  export HOME="$DATADIR"
  export GODEBUG="madvdontneed=1"
}

# WebUI 的 ksu.exec 跑在管理器 App 里，直接 start 不受 ksud 保护。
# WebUI 启停走 ksud module action（与模块 Action 按钮同一通道）。
ksud_bin() {
  for b in /data/adb/ksu/bin/ksud /data/adb/ksud /data/adb/apd; do
    [ -x "$b" ] && { echo "$b"; return 0; }
  done
  return 1
}

# 通过 ksud 执行模块 Action（与管理器 Action 按钮同一条通道）。
ksud_action() {
  k=$(ksud_bin) || { echo "ksud_not_found"; return 1; }
  echo "via $($k --version 2>/dev/null | head -n 1 || echo ksud)"
  "$k" module action wb2api_panel
}

start_svc() {
  pid=$(running_pid) && { echo "already_running pid=$pid"; return 0; }
  [ -x "$BIN" ] || { echo "missing_binary"; return 1; }
  [ -f "$CFG" ] || { echo "missing_config"; return 1; }
  apply_env
  rotate_log
  chmod 600 "$CFG" 2>/dev/null
  cd "$DATADIR" || return 1
  if command -v setsid >/dev/null 2>&1; then
    setsid "$BIN" -config "$CFG" >>"$LOG" 2>&1 &
  else
    nohup "$BIN" -config "$CFG" >>"$LOG" 2>&1 &
  fi
  echo $! > "$PIDFILE"
  n=0
  while [ "$n" -lt 8 ]; do
    pid=$(cat "$PIDFILE" 2>/dev/null)
    if is_our_pid "$pid"; then
      if health_ok; then
        echo "started pid=$pid"
        return 0
      fi
    else
      echo "start_failed"
      tail -n 15 "$LOG" 2>/dev/null
      return 1
    fi
    n=$((n + 1))
    sleep 1
  done
  if is_our_pid "$(cat "$PIDFILE" 2>/dev/null)"; then
    echo "started pid=$(cat "$PIDFILE") (health_pending)"
    return 0
  fi
  echo "start_failed"
  tail -n 15 "$LOG" 2>/dev/null
  return 1
}

stop_svc() {
  pid=$(running_pid)
  if [ -n "$pid" ]; then
    kill -TERM "$pid" 2>/dev/null
    n=0
    while [ "$n" -lt 20 ] && [ -d "/proc/$pid" ]; do
      n=$((n + 1))
      sleep 1
    done
    [ -d "/proc/$pid" ] && kill -KILL "$pid" 2>/dev/null
  fi
  rm -f "$PIDFILE"
  echo "stopped"
}

status_svc() {
  pid=$(running_pid)
  if [ -n "$pid" ]; then
    echo "running=1"
    echo "pid=$pid"
    if health_ok; then echo "health=ok"; else echo "health=starting"; fi
  else
    echo "running=0"
    echo "pid="
    echo "health=down"
  fi
  echo "listen=$(listen_addr)"
  echo "port=$(listen_port)"
  if [ -f "$DATADIR/api_key.txt" ]; then
    k=$(head -n 1 "$DATADIR/api_key.txt")
  else
    k=$(sed -n 's/.*"api_key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CFG" 2>/dev/null | head -n 1)
  fi
  echo "api_key=$k"
  ips=$(ip -o -4 addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | tr '\n' ' ')
  echo "ips=${ips}"
}

print_key() {
  if [ -f "$DATADIR/api_key.txt" ]; then
    head -n 1 "$DATADIR/api_key.txt"
    return 0
  fi
  sed -n 's/.*"api_key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CFG" 2>/dev/null | head -n 1
}

case "${1:-}" in
  start) start_svc ;;
  stop) stop_svc ;;
  restart) stop_svc; sleep 1; start_svc ;;
  ksud-start|ksud-restart) ksud_action ;;
  status) status_svc ;;
  running) running_pid >/dev/null ;;
  health) health_ok ;;
  key) print_key ;;
  *) echo "usage: $0 start|stop|restart|ksud-start|status|running|health|key"; exit 1 ;;
esac