#!/system/bin/sh
MODDIR=${0%/*}
export TZ=Asia/Shanghai
echo "重启 WorkBuddy2API ..."
"$MODDIR/scripts/control.sh" restart
sleep 1
"$MODDIR/scripts/control.sh" status
echo "面板: http://127.0.0.1:7863/panel/"