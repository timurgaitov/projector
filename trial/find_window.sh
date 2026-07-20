#!/bin/sh
# Per-second video bitrate of $1; prints busiest 60s window and global 1s peak.
ffprobe -v error -select_streams v:0 -show_entries packet=pts_time,size -of csv=p=0 "$1" |
awk -F, '
  $1 != "" { sec = int($1); bytes[sec] += $2; if (sec > maxsec) maxsec = sec }
  END {
    peak1 = 0; for (s = 0; s <= maxsec; s++) if (bytes[s] > peak1) { peak1 = bytes[s]; peak1s = s }
    win = 0; for (s = 0; s < 60 && s <= maxsec; s++) win += bytes[s]
    best = win; bestat = 0
    for (s = 60; s <= maxsec; s++) { win += bytes[s] - bytes[s-60]; if (win > best) { best = win; bestat = s - 59 } }
    printf "peak 1s: %.1f Mbps at t=%ds\nbusiest 60s window: starts t=%ds, avg %.1f Mbps\n", peak1*8/1e6, peak1s, bestat, best*8/60e6
  }'
