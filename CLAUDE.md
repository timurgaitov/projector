# project

Personal Go tool: stream a local video from the Mac to a Wanbo Mozart 1 Pro projector
(Google TV / VLC for Android) over the LAN. Single stdlib-only binary, no deps.

## Build / run
- `go build -o ~/.local/bin/project .` — build to PATH (module name is `project`; go.mod required)
- `project <file> [bitrate]` — transcode-to-fit (default 6M), serve at http://<mac-ip>:1111/
- `project -raw <file>` — serve as-is, skip transcode
- Output `<name>-ready.mp4` beside the source; reused if newer than source (delete to force re-encode)

## Architecture (main.go)
- Transcode: shells to `ffmpeg` (`-hide_banner -loglevel error -nostats`, `h264_videotoolbox`, 1080p cap / H.264 High / AAC / faststart)
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
