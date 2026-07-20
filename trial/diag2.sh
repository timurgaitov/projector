#!/bin/sh
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
MAC_IP=$(ipconfig getifaddr en0)
TRIAL=/tmp/ladder-trial
for clip in v4-mp4-ac3copy.mp4 v1-mp4-aac.mp4 v3-toolpath.mp4; do
  rm -rf "$TRIAL"; mkdir -p "$TRIAL"
  ln -s "$DIR/$clip" "$TRIAL/t-ready.mp4"; touch "$TRIAL/t.mp4"
  "$HOME/.local/bin/project" "$TRIAL/t.mp4" >/dev/null 2>&1 &
  SRV=$!
  curl -s --retry-connrefused --retry 20 --retry-delay 1 -o /dev/null http://127.0.0.1:1111/
  adb logcat -c
  adb shell "am start -a android.intent.action.VIEW -d \"http://$MAC_IP:1111/t.mp4\" -t \"video/mp4\" org.videolan.vlc" >/dev/null 2>&1
  sleep 50
  LOG=$(adb logcat -d 2>/dev/null)
  LATE=$(printf '%s' "$LOG" | grep -c "too late")
  DROPS=$(printf '%s' "$LOG" | grep -iE "VLC.*drop|codec.*drop" | grep -cv "mali_gralloc")
  echo "RESULT $clip: late=$LATE vlc_drops=$DROPS"
  adb shell 'am force-stop org.videolan.vlc' >/dev/null 2>&1
  kill $SRV 2>/dev/null
  sleep 3
done
rm -rf "$TRIAL"; echo DIAG2-DONE
