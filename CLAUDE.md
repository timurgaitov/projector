# project

Personal Go tool: stream a local video from the Mac to a Wanbo Mozart 1 Pro projector
(Google TV / VLC for Android) over the LAN. Single stdlib-only binary, no deps.

## Build / run
- `go build -o ~/.local/bin/project .` — build to PATH (module name is `project`; go.mod required)
- `project <file>` — the only command; no flags. Probes, does the least work, serves at http://<mac-ip>:1111/
- Output `<name>-ready.mp4` beside the source (atomic: encodes to `.part`, renames on success); reused if newer than source (delete to force re-encode)

## Architecture (main.go)
- `plan()` ffprobe's the input, then picks the cheapest ffmpeg mode: **copy** (already h264 ≤1080p ≤6.5Mbps + aac → lossless remux), **audio-only** (copy video, re-encode audio to aac), or **full** (re-encode video+audio via `h264_videotoolbox` to 6M / 1080p / H.264 High). Probe failure → full transcode. All outputs are faststart MP4.
- Serve: `http.ServeContent` (streams from disk, Range/206 seek); the one file answers at any path incl. `/`
- Discover: minimal UPnP/DLNA MediaServer — SSDP (answers M-SEARCH) + ContentDirectory SOAP; shows in VLC → Local Network as "project (Mac)"

## Testing
- Test clip: `ffmpeg -f lavfi -i testsrc=duration=2:size=1280x720:rate=25 -f lavfi -i sine=duration=2 -pix_fmt yuv420p -c:v libx264 -c:a aac out.mkv`
- Run binary in background, then wait for it: `curl -s --retry-connrefused --retry 40 --retry-delay 1 -o /dev/null http://127.0.0.1:1111/`
- SSDP: send an `M-SEARCH` UDP packet to 239.255.255.250:1900; expect a reply carrying `LOCATION`

## Gotchas
- Python `http.server` has NO Range support → breaks video seeking (why serving is custom Go)
- macOS TCC blocks terminal reads of `~/Downloads` and `~/Documents` (`~/Desktop` is fine)
- DLNA discovery needs LAN multicast; some Wi-Fi APs block it → direct URL http://<ip>:1111/ is the fallback
