#!/bin/sh
# Build the bitrate-ladder trial clips from a real BDRip excerpt.
# Usage: build.sh <source>
# Rungs: h264 + HEVC at 12/20/30/50 Mbps sustained (CBR-ish: maxrate=target,
# bufsize=target/2 so the 1s peak tracks the target), one HEVC 10-bit rung,
# and two untouched source excerpts ("copy" rungs — the real serve-as-is case):
# one around the movie's global 31 Mbps 1s peak (t=1365), one of the busiest
# sustained minute (t=7713, 15.4 Mbps avg). Encode rungs use the sustained
# window so real grain/motion forces the encoder to spend its budget.
set -e
SRC=$1; START=7713; DUR=60
DIR=$(dirname "$0")
AUDIO="-map 0:a:0 -c:a aac -b:a 192k -ac 2"
COMMON="-ss $START -t $DUR -map 0:v:0 -movflags +faststart -y"

ffmpeg -v error -ss 1335 -i "$SRC" -t $DUR -map 0:v:0 -map 0:a:0 \
  -c:v copy -c:a aac -b:a 192k -ac 2 -movflags +faststart -y "$DIR/copy-burst.mp4"
ffmpeg -v error -ss "$START" -i "$SRC" -t $DUR -map 0:v:0 -map 0:a:0 \
  -c:v copy -c:a aac -b:a 192k -ac 2 -movflags +faststart -y "$DIR/copy-sustained.mp4"
echo "copy rungs done"

for M in 12 20 30 50; do
  ffmpeg -v error -i "$SRC" $COMMON $AUDIO \
    -c:v libx264 -preset fast -b:v ${M}M -maxrate ${M}M -bufsize $((M/2))M \
    -pix_fmt yuv420p -g 48 "$DIR/h264-${M}M.mp4"
  echo "h264-${M}M done"
done

for M in 12 20 30 50; do
  ffmpeg -v error -i "$SRC" $COMMON $AUDIO \
    -c:v libx265 -preset fast -b:v ${M}M -maxrate ${M}M -bufsize $((M/2))M \
    -pix_fmt yuv420p -tag:v hvc1 "$DIR/hevc-${M}M.mp4"
  echo "hevc-${M}M done"
done

ffmpeg -v error -i "$SRC" $COMMON $AUDIO \
  -c:v libx265 -preset fast -b:v 30M -maxrate 30M -bufsize 15M \
  -pix_fmt yuv420p10le -tag:v hvc1 "$DIR/hevc10-30M.mp4"
echo "hevc10-30M done"
echo "ALL BUILT"
