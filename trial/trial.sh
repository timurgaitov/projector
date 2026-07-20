#!/bin/sh
# Bitrate-ladder trial: plays every ladder clip on the projector via VLC and
# collects objective verdicts from logcat + media_session. No eyeballing needed.
# Usage: trial.sh   (projector on, wireless debugging enabled)
# Per clip: PASS = hardware decoder (c2.amlogic), 0 drop/late lines, and the
# projector kept consuming bytes at realtime rate (en0 obytes delta vs the
# clip's size-proportional expectation; VLC only caches ~1.5s, so a stalled
# pipeline stops pulling). media_session position is NOT used — VLC only
# snapshots it on state changes, so it reads 0 mid-playback.
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
MAC_IP=$(ipconfig getifaddr en0)
PLAY=50           # seconds of playback per clip
TRIAL=/tmp/ladder-trial

adb connect 192.168.1.8:5555 >/dev/null 2>&1
if ! adb devices | grep -q "device$"; then
  T=$(adb mdns services 2>/dev/null | grep "_adb-tls-connect" | head -1 | awk '{print $NF}')
  [ -n "$T" ] && adb connect "$T" >/dev/null 2>&1
fi
adb devices | grep -q "device$" || { echo "FATAL: projector not reachable over adb"; exit 1; }
echo "clip,decoder,drop_lines,late_lines,mb_served,mb_expected,verdict"

for clip in "$DIR"/h264-12M.mp4 "$DIR"/h264-20M.mp4 "$DIR"/h264-30M.mp4 "$DIR"/h264-50M.mp4 \
            "$DIR"/hevc-12M.mp4 "$DIR"/hevc-20M.mp4 "$DIR"/hevc-30M.mp4 "$DIR"/hevc-50M.mp4 \
            "$DIR"/hevc10-30M.mp4 "$DIR"/copy-sustained.mp4 "$DIR"/copy-burst.mp4; do
  [ -f "$clip" ] || { echo "$(basename "$clip"),MISSING,,,,,SKIP"; continue; }
  rm -rf "$TRIAL"; mkdir -p "$TRIAL"
  ln -s "$clip" "$TRIAL/t-ready.mp4"; touch "$TRIAL/t.mp4"
  "$HOME/.local/bin/project" "$TRIAL/t.mp4" >/dev/null 2>&1 &
  SRV=$!
  curl -s --retry-connrefused --retry 20 --retry-delay 1 -o /dev/null "http://127.0.0.1:1111/" || {
    echo "$(basename "$clip"),SERVE_FAIL,,,,,SKIP"; kill $SRV 2>/dev/null; continue; }

  adb logcat -c
  B0=$(netstat -ib -I en0 | awk 'NR==2{print $10}')
  adb shell "am start -a android.intent.action.VIEW -d \"http://$MAC_IP:1111/t.mp4\" -t \"video/mp4\" org.videolan.vlc" >/dev/null 2>&1
  sleep $PLAY

  B1=$(netstat -ib -I en0 | awk 'NR==2{print $10}')
  MB_SERVED=$(( (B1 - B0) / 1000000 ))
  CLIP_MB=$(( $(stat -f %z "$clip") / 1000000 ))
  MB_EXPECT=$(( CLIP_MB * PLAY / 60 ))
  LOG=$(adb logcat -d 2>/dev/null)
  DEC=$(printf '%s' "$LOG" | grep -o "using c2\.[a-z0-9._-]*" | sort -u | tr '\n' ' ')
  DROPS=$(printf '%s' "$LOG" | grep -i "drop" | grep -cv "mali_gralloc")
  LATE=$(printf '%s' "$LOG" | grep -c "too late")

  VERDICT=PASS
  case "$DEC" in *amlogic*) : ;; *) VERDICT=FAIL_SW_DECODE ;; esac
  [ "$DROPS" -gt 0 ] && VERDICT=FAIL_DROPS
  [ "$LATE" -gt 0 ] && VERDICT=FAIL_LATE
  [ "$MB_SERVED" -lt $(( MB_EXPECT * 6 / 10 )) ] && VERDICT=FAIL_STALLED
  echo "$(basename "$clip"),${DEC:-none},$DROPS,$LATE,$MB_SERVED,$MB_EXPECT,$VERDICT"

  adb shell 'am force-stop org.videolan.vlc' >/dev/null 2>&1
  kill $SRV 2>/dev/null; wait $SRV 2>/dev/null
  sleep 3
done
rm -rf "$TRIAL"
echo "DONE"
