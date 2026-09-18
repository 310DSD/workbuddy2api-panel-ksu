#!/system/bin/sh
# late_start：等开机完成后拉起网关，并做崩溃重启
MODDIR=${0%/*}
CTRL="$MODDIR/scripts/control.sh"
export TZ=Asia/Shanghai

i=0
while [ "$(getprop sys.boot_completed 2>/dev/null)" != "1" ] && [ "$i" -lt 90 ]; do
  i=$((i + 1))
  sleep 2
done

fail=0
while [ ! -f "$MODDIR/disable" ] && [ ! -f "$MODDIR/remove" ]; do
  if "$CTRL" running >/dev/null 2>&1; then
    fail=0
    sleep 20
    continue
  fi
  "$CTRL" start >/dev/null 2>&1
  fail=$((fail + 1))
  [ "$fail" -gt 6 ] && fail=6
  sleep $((fail * 8))
done