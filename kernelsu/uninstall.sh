#!/system/bin/sh
MODDIR=${0%/*}
DATADIR=/data/adb/wb2api
"$MODDIR/scripts/control.sh" stop >/dev/null 2>&1
if [ -f "$DATADIR/.wipe_on_uninstall" ]; then
  rm -rf "$DATADIR"
fi