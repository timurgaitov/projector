# project

Personal Go tool: stream a local video from the Mac to a Wanbo Mozart 1 Pro projector
(Google TV / VLC for Android) over the LAN. Single stdlib-only binary, no deps.

## Build / run
- `go build -o ~/.local/bin/project .` — build to PATH (module name is `project`; go.mod required)
- `project <file>` — the only command; no flags. Probes, does the least work, serves at http://<mac-ip>:1111/
- Output `<name>-ready.mp4` beside the source (atomic: encodes to `.part`, renames on success); if it exists it's served as-is, no questions asked (delete it to force a re-encode). An input that's already a fit mp4 is served as-is — no ffmpeg run, no output file.

## Architecture (main.go)
- `plan()` ffprobe's the input and decides per stream: video is "fit" if 8-bit 4:2:0 h264 ≤1080p with projected bitrate ≤12Mbps (≈ the proven link rate; derived from size×8/duration when the container omits bit_rate; re-encoded audio's weight is swapped for the 256k AAC target) *and* its measured 1s-window peak ≤12Mbps (`peakBitrate()`: full-file packet scan, ~1 min for 12GB — averages hide scene bursts that stall the link), audio is fit if aac. Fit streams are copied, unfit ones re-encoded (audio → aac_at 256k, downmixed to stereo via `aformat`, loudness-normalized to −16 LUFS by a static gain from `loudnessGain()` — an EBU R128 measure pass over the same downmix — with an `alimiter` ceiling at −1.5 dB catching only split-second overs) (`hevc_videotoolbox` 8M, `hvc1` tag — same budget, more quality; hw decode verified on-device; no pinned pix_fmt so 10-bit sources come out Main10; scaled into a 1920x1080 box). Streams are mapped by absolute index — skips attached_pic cover art; audio-less inputs work. Multi-audio inputs: a stdin picker lists every track (lang/codec/layout/kbps/title) and takes numbers in output order — first kept becomes the default disposition (others cleared); Enter/EOF/non-tty keeps just the input's default track (old behavior). Each re-encoded track gets its own loudness pass + per-stream `-filter:a:N`; unselected tracks' bitrate is subtracted from the projected total; keeping a subset (or reordering) of an otherwise-fit mp4 triggers a lossless remux. mkv `title` tags don't survive into mp4 — they're carried as `handler_name` (which the mov muxer writes and VLC's track menu shows). Copy failures (bad timestamps etc.) retry as a full transcode; probe failure → full transcode. All encoded outputs are faststart MP4.
- Serve: `http.ServeContent` (streams from disk, Range/206 seek); the one file answers at any path incl. `/`
- Discover: minimal UPnP/DLNA MediaServer — SSDP (answers M-SEARCH) + ContentDirectory SOAP; shows in VLC → Local Network as "project (Mac)"

## Testing
- Test clip: `ffmpeg -f lavfi -i testsrc=duration=2:size=1280x720:rate=25 -f lavfi -i sine=duration=2 -pix_fmt yuv420p -c:v libx264 -c:a aac out.mkv`
- Run binary in background, then wait for it: `curl -s --retry-connrefused --retry 40 --retry-delay 1 -o /dev/null http://127.0.0.1:1111/`
- SSDP: send an `M-SEARCH` UDP packet to 239.255.255.250:1900; expect a reply carrying `LOCATION`
- Probing projector codec support (how HEVC hw decode was verified, 2026-07):
  1. `adb connect 192.168.1.8:5555` (IP may drift — rediscover with `adb mdns services`; wireless debugging is enabled, Mac's key authorized). Paper answer: `getprop ro.board.platform` etc.; vendor codec tables in `/vendor/etc/media_codecs*.xml` list every hw decoder with size/rate limits (but not bit depth — test that live).
  2. Smuggle any codec past plan(): encode the test clip straight to `x-ready.mp4` (e.g. `-c:v hevc_videotoolbox -tag:v hvc1`, add `noise=alls=20:allf=t` so it's not trivially decodable; `-pix_fmt p010le` for the 10-bit variant), `touch x.mp4`, `project x.mp4` — the -ready file is served untouched, no fit check.
  3. Play it on the projector, no remote needed: `adb logcat -c`, then `adb shell 'am start -a android.intent.action.VIEW -d "http://<mac-ip>:1111/x.mp4" -t "video/mp4" org.videolan.vlc'`
  4. Verdict: `adb logcat -d | grep "using c2\."` — `c2.amlogic.*` = hardware, `c2.android.*`/`OMX.google.*` = software fallback; also grep `-i drop`. Ignore `mali_gralloc` "falling back" lines (GPU allocator noise). Synthetic pass ≠ real-rip pass — confirm with real content before widening plan().

## Projector hardware (probed via adb, 2026-07)
- Google TV side is a built-in SkyworthDigital "4K Google TV Stick": Amlogic SoC (board HP46B), Android 14, armeabi-v7a. `adb connect 192.168.1.8:5555` (wireless debugging; Mac's key authorized).
- Hardware video decoders (vendor codec table): h264 + HEVC + VP9 + AV1, all to 4K. Verified on-device (VLC playing an HTTP stream, logcat): 1080p HEVC 8-bit *and* 10-bit both use hardware `c2.amlogic.hevc.decoder` — no software fallback, no drops. Vendor audio decoders: AC-3, E-AC-3, AC-4, DTS(-HD).
- Implication: plan() could accept ≤1080p 4:2:0 HEVC as copy-fit (same bitrate gates, `-tag:v hvc1` in mp4) — pending a real-rip trial; synthetic clips passed.

## Gotchas
- Link speed (iperf3, 2026-07-20): 266–270 Mbps sustained TCP Mac→stick (3×30s runs, ≤3 retransmits; 4 streams ≈300M aggregate; UDP at 60M: 0.009% loss, <1 ms jitter). The radio is NOT the bottleneck — the earlier "~11.7 Mbps sustained proven" stress-clip figure (2026-07) was measured through VLC, so it bounded the whole player pipeline (or that day's radio), not the link. Keep the 12M gate until the BDRip-burst replay (next bullet) passes. Recipe: `iperf3 -s -1` on the Mac (brew, v3.21); matching static arm32 binary lives on the stick at `/data/local/tmp/iperf3` (userdocs/iperf3-static arm32v7): `adb shell '/data/local/tmp/iperf3 -c <mac-ip> -R -t 30'` — `-R` is load-bearing (default direction is stick→Mac upload). Stick has no wget/curl; `adb push` doubles as a crude throughput check (~54 MB/s).
- Encoder behavior: stress clip = `testsrc2` + `noise=alls=N` through `h264_videotoolbox` (N≈32→7M, 33→8M, 36-37→11-12M; bitrate is content-capped, `-b:v` barely steers it — hevc_videotoolbox behaves the same: 8M target → ~5.6M on noisy 1080p, ~3.3× realtime). Do NOT use x264 `nal-hrd=cbr` filler streams — the projector's decoder shows one frame and quits. videotoolbox overshoots `-maxrate` up to 2-3× on complex content.
- Real x264 BDRips burst ~2× their average: a 10.5 Mbps-avg movie hit 20.4 Mbps over a 1s window (18 Mbps sustained for 10s) and dropped frames on the projector despite passing the 12M average gate — why plan() measures the peak instead of trusting averages or (usually absent) `max_bit_rate` metadata. With 266M of measured link headroom those drops were likely the player pipeline (VLC buffering/demux on the stick), not radio — replay a bursty clip through VLC before raising any gate
- A bare `-ac 2` downmix of 5.1 plays ~6-7 dB quieter than the source (swresample clip-protection scales the mix down; dialogue suffers most) — why re-encoded audio gets the measured loudness gain. Measure on the *downmixed* signal, not the original channels.
- Do NOT cap the loudness gain by whole-file peak: movie downmixes routinely peak over 0 dBTP (a real BDRip: −27 LUFS, +1.8 dBTP), so a peak cap inverts an +11 dB boost into a −3.3 dB cut. Boost fully, limit transients (`alimiter` — its `level` option defaults to *true* and would re-normalize away the gain; set `level=false:latency=true`).
- Python `http.server` has NO Range support → breaks video seeking (why serving is custom Go)
- macOS TCC blocks terminal reads of `~/Downloads` and `~/Documents` (`~/Desktop` is fine)
- adb shell: single-quote the whole remote command (nested double quotes / globs get mangled); this zsh also expands bare `=word` args (`echo ===` errors)
- Wireless debugging turns itself OFF on every projector reboot — after a power cycle `adb connect` gets "connection refused" until it's re-enabled on-screen (Settings → System → Developer options). No re-pairing needed; :5555 kept working after the toggle
- DLNA discovery needs LAN multicast; some Wi-Fi APs block it → direct URL http://<ip>:1111/ is the fallback
