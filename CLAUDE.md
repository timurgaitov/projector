# project

Personal Go tool: stream a local video from the Mac to a Wanbo Mozart 1 Pro projector
(Google TV / VLC for Android) over the LAN. Single stdlib-only binary, no deps.

## Build / run
- `go build -o ~/.local/bin/project .` — build to PATH (module name is `project`; go.mod required)
- `project <file>` — the only command; no flags. Probes, does the least work, serves at http://<mac-ip>:1111/
- Output `<name>-ready.mp4` beside the source (atomic: encodes to `.part`, renames on success); if it exists it's served as-is, no questions asked (delete it to force a re-encode). An input that's already a fit mp4 is served as-is — no ffmpeg run, no output file.

## Architecture (main.go)
- `plan()` ffprobe's the input and decides per stream: video is "fit" if 8-bit 4:2:0 h264 ≤1080p with projected bitrate ≤12Mbps (≈ the proven link rate; derived from size×8/duration when the container omits bit_rate; re-encoded audio's weight is swapped for the 256k AAC target) *and* its measured 1s-window peak ≤12Mbps (`peakBitrate()`: full-file packet scan, ~1 min for 12GB — averages hide scene bursts that stall the link), audio is fit if aac. Fit streams are copied, unfit ones re-encoded (audio → aac_at 256k, downmixed to stereo) (`h264_videotoolbox` 8M — enough at 1080p; scaled into a 1920x1080 box). Streams are mapped by absolute index — skips attached_pic cover art; audio-less inputs work. Copy failures (bad timestamps etc.) retry as a full transcode; probe failure → full transcode. All encoded outputs are faststart MP4.
- Serve: `http.ServeContent` (streams from disk, Range/206 seek); the one file answers at any path incl. `/`
- Discover: minimal UPnP/DLNA MediaServer — SSDP (answers M-SEARCH) + ContentDirectory SOAP; shows in VLC → Local Network as "project (Mac)"

## Testing
- Test clip: `ffmpeg -f lavfi -i testsrc=duration=2:size=1280x720:rate=25 -f lavfi -i sine=duration=2 -pix_fmt yuv420p -c:v libx264 -c:a aac out.mkv`
- Run binary in background, then wait for it: `curl -s --retry-connrefused --retry 40 --retry-delay 1 -o /dev/null http://127.0.0.1:1111/`
- SSDP: send an `M-SEARCH` UDP packet to 239.255.255.250:1900; expect a reply carrying `LOCATION`

## Gotchas
- Link testing (2026-07): ~11.7 Mbps sustained proven over 5 GHz Wi-Fi. Stress clip = `testsrc2` + `noise=alls=N` through `h264_videotoolbox` (N≈32→7M, 33→8M, 36-37→11-12M; bitrate is content-capped, `-b:v` barely steers it). Do NOT use x264 `nal-hrd=cbr` filler streams — the projector's decoder shows one frame and quits. videotoolbox overshoots `-maxrate` up to 2-3× on complex content.
- Real x264 BDRips burst ~2× their average: a 10.5 Mbps-avg movie hit 20.4 Mbps over a 1s window (18 Mbps sustained for 10s) and dropped frames on the projector despite passing the 12M average gate — why plan() measures the peak instead of trusting averages or (usually absent) `max_bit_rate` metadata
- Python `http.server` has NO Range support → breaks video seeking (why serving is custom Go)
- macOS TCC blocks terminal reads of `~/Downloads` and `~/Documents` (`~/Desktop` is fine)
- DLNA discovery needs LAN multicast; some Wi-Fi APs block it → direct URL http://<ip>:1111/ is the fallback
