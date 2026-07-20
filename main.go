// project: fit ONE video file for the projector, serve it over HTTP (with
// Range), and advertise it as a minimal UPnP/DLNA MediaServer so VLC's
// "Local Network" browser discovers it. No external daemon, stdlib only.
//
// usage: project <file>
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const uuid = "uuid:6f3a2b10-0000-4a00-8000-project0dlna1" // stable across runs

// Fixed "fit" target: the projector is native 1080p. The 2026-07-20 ladder
// trial (real BDRip content, logcat-verified on-device) played h264 up to
// 60M and HEVC up to 57M 1s-peaks clean on hardware decode, with iperf3
// showing 266 Mbps of link. The gates sit under those proven peaks with
// margin for worse radio days; the old 12M gate traced to a VLC-pipeline
// artifact, not the link. maxOverall must stay above targetBitrate +
// audioBitrate, or fit files would be re-encoded UP.
const (
	targetBitrate = "12M"      // video bitrate when a full transcode is needed
	targetBufsize = "24M"      // decoder buffer: 2× targetBitrate
	audioBitrate  = 256_000    // bits/s; AAC target when audio is re-encoded
	targetLUFS    = -16.0      // integrated loudness target for re-encoded audio
	maxTruePeak   = -1.5       // dB ceiling the limiter holds re-encoded audio under
	maxOverall    = 40_000_000 // bits/s; inputs at/under this average are "fits"
	maxPeak       = 50_000_000 // bits/s; worst measured 1s window a copy may carry
	maxWidth      = 1920
	maxHeight     = 1080
)

var (
	filePath string
	title    string
	port     string
	ip       string
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: project <file>")
		os.Exit(1)
	}
	input := os.Args[1]
	port = "1111"
	title = baseName(input)

	filePath = fit(input)
	fi, err := os.Stat(filePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ip = localIP()

	http.HandleFunc("/media", serveMedia(fi.ModTime()))
	http.HandleFunc("/", serveMedia(fi.ModTime())) // bare URL also plays the file
	http.HandleFunc("/description.xml", serveXML(deviceXML))
	http.HandleFunc("/ContentDirectory.xml", serveXML(scpdXML))
	http.HandleFunc("/ctl/ContentDirectory", serveControl)
	http.HandleFunc("/evt/ContentDirectory", func(w http.ResponseWriter, r *http.Request) {})

	go ssdp() // discovery

	fmt.Printf("serving %s (%d bytes)\n", filePath, fi.Size())
	fmt.Printf("  HTTP : http://%s:%s/\n", ip, port)
	fmt.Printf("  DLNA : \"%s\" — look under VLC → Local Network\n", "project (Mac)")
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// fit returns a projector-ready file for the input, doing the least work
// needed: probe first, then serve as-is / remux / re-encode audio / transcode.
// Anything encoded lands in <name>-ready.mp4 next to the input (faststart MP4,
// HEVC + AAC); an input that already fits in an mp4 container is served as-is.
func fit(in string) string {
	out := filepath.Join(filepath.Dir(in), baseName(in)+"-ready.mp4")

	// An existing -ready.mp4 is trusted as-is — stale or off-target leftovers
	// are the user's to delete.
	if _, err := os.Stat(out); err == nil {
		fmt.Printf("▶ Reusing %s (delete it to force a re-encode)\n", filepath.Base(out))
		return out
	}

	d := plan(in)
	if isFit(d) {
		fmt.Printf("▶ Already fit — serving %s as-is\n", filepath.Base(in))
		return in
	}
	switch {
	case !d.ok || !d.vCopy:
		fmt.Printf("▶ Transcoding (%s) → %s\n", d.why, filepath.Base(out))
	case !allCopy(d.audios):
		fmt.Printf("▶ Video is fine — re-encoding audio only (audio %s) → %s\n", reencNames(d.audios), filepath.Base(out))
	default:
		fmt.Printf("▶ Already fit — remuxing (lossless) → %s\n", filepath.Base(out))
	}

	// Encode to a .part file and rename on success, so an interrupted/failed run
	// never leaves a half-encoded <name>-ready.mp4 for the cache to reuse.
	tmp := out + ".part"
	err := ffmpeg(in, tmp, codecArgs(d), d.durSec)
	if err != nil && d.ok && (d.vCopy || anyCopy(d.audios)) {
		// Streams that look fit can still refuse to copy into mp4 (bad
		// timestamps, packed bitstreams); re-encoding regenerates them.
		fmt.Fprintln(os.Stderr, "▶ Copy failed — retrying as a full transcode")
		retry := d
		retry.vCopy = false
		retry.audios = append([]aTrack(nil), d.audios...)
		for i := range retry.audios {
			retry.audios[i].copy = false
		}
		err = ffmpeg(in, tmp, codecArgs(retry), d.durSec)
	}
	if err != nil {
		os.Remove(tmp)
		fmt.Fprintln(os.Stderr, "ffmpeg failed:", err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		fmt.Fprintln(os.Stderr, "rename failed:", err)
		os.Exit(1)
	}
	return out
}

// isFit reports whether a probed file can be served without running ffmpeg at
// all: already an mp4 whose video (and audio, if any) would be plain copies,
// with every audio track kept in file order — dropping or reordering tracks
// needs a remux even when each stream is copy-clean.
func isFit(d decision) bool {
	return d.ok && d.isMP4 && d.vCopy && d.allAudio && allCopy(d.audios)
}

func allCopy(ts []aTrack) bool {
	for _, t := range ts {
		if !t.copy {
			return false
		}
	}
	return true
}

func anyCopy(ts []aTrack) bool {
	for _, t := range ts {
		if t.copy {
			return true
		}
	}
	return false
}

// reencNames lists the codecs of the tracks a re-encode will touch, for messages.
func reencNames(ts []aTrack) string {
	var names []string
	for _, t := range ts {
		if !t.copy {
			names = append(names, t.name)
		}
	}
	return strings.Join(names, "+")
}

func ffmpeg(in, tmp string, codec []string, durSec float64) error {
	start := time.Now()
	argv := append([]string{"-hide_banner", "-loglevel", "error", "-nostats",
		"-progress", "pipe:1", "-stats_period", "0.5",
		"-y", "-i", in}, codec...)
	argv = append(argv, "-movflags", "+faststart", "-f", "mp4", tmp)
	cmd := exec.Command("ffmpeg", argv...)
	cmd.Stderr = os.Stderr // ffmpeg is quiet now; only real errors reach here
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	showProgress(out, durSec)
	err = cmd.Wait()
	if err == nil {
		fmt.Printf("▶ Done in %s\n", took(start))
	}
	return err
}

// showProgress renders ffmpeg's -progress key=value feed as one line updated
// in place: percent and ETA when the input duration is known, otherwise just
// position and encode speed. Values within a block arrive in a fixed order
// ending with "progress=", so that key triggers the redraw.
func showProgress(r io.Reader, durSec float64) {
	shown := false
	parseProgress(r, func(pos, speed float64) {
		line := fmt.Sprintf("▶ %s encoded", fmtSec(pos))
		if durSec > 0 {
			line = fmt.Sprintf("▶ %3.0f%%", min(pos/durSec, 1)*100)
			if left := durSec - pos; speed > 0 && left/speed >= 0.5 {
				line += fmt.Sprintf("  ~%s left", fmtSec(left/speed))
			}
		}
		if speed > 0 {
			line += fmt.Sprintf("  (%.1fx)", speed)
		}
		fmt.Printf("\r%-40s", line)
		shown = true
	})
	if shown {
		fmt.Println()
	}
}

// parseProgress streams ffmpeg's -progress key=value feed, calling report once
// per progress block with the position (seconds) and speed (× realtime).
func parseProgress(r io.Reader, report func(pos, speed float64)) {
	var pos, speed float64
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "out_time_us":
			if us, err := strconv.ParseFloat(v, 64); err == nil {
				pos = us / 1e6
			}
		case "speed":
			speed, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), "x"), 64)
		case "progress":
			report(pos, speed)
		}
	}
}

func fmtSec(s float64) string {
	return (time.Duration(s * float64(time.Second))).Round(time.Second).String()
}

// took formats a step's elapsed wall time for log lines.
func took(start time.Time) string {
	d := time.Since(start)
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// decision is plan()'s verdict: which streams to use and how little work each
// needs. The zero value (ok=false) means "probe failed, assume the worst".
type decision struct {
	ok       bool
	vIdx     int      // absolute video stream index
	vCodec   string   // video codec name (steers the mp4 tag when copying hevc)
	vCopy    bool     // video already fits the target → plain copy
	audios   []aTrack // kept audio tracks, in output order; empty → no audio
	allAudio bool     // kept every input audio track, in file order
	isMP4    bool     // container is already mp4/mov
	why      string   // short reason when the video needs a transcode
	durSec   float64  // input duration in seconds; 0 if unknown
}

// aTrack is one audio stream of the input plus plan()'s verdict for it.
type aTrack struct {
	idx      int    // absolute stream index
	name     string // codec name
	lang     string // language tag; "" or "und" when absent
	layout   string // channel layout, e.g. "5.1(side)"
	channels int
	title    string
	def      bool    // marked default in the input
	br       int     // bits/s; 0 when the container doesn't say
	copy     bool    // already aac → plain copy
	gainDB   float64 // static loudness gain when re-encoded
	hasGain  bool    // gainDB was actually measured
}

// plan probes the input with ffprobe and decides, per stream, whether a copy
// suffices. Video fits if it's 8-bit 4:2:0 h264 within 1080p and the projected
// output bitrate stays under maxOverall — both the average and the measured
// 1-second peak (a full-file packet scan). Audio fits if it's already aac;
// when the input carries several audio tracks the user picks which to keep.
// Any probe failure falls back to a full transcode (safe default).
func plan(in string) decision {
	d := decision{}
	out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", in).Output()
	if err != nil {
		d.why = "probe failed"
		return d
	}
	var p struct {
		Streams []struct {
			Index         int    `json:"index"`
			CodecType     string `json:"codec_type"`
			CodecName     string `json:"codec_name"`
			PixFmt        string `json:"pix_fmt"`
			Width         int    `json:"width"`
			Height        int    `json:"height"`
			BitRate       string `json:"bit_rate"`
			Channels      int    `json:"channels"`
			ChannelLayout string `json:"channel_layout"`
			Tags          struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
				Default     int `json:"default"`
			} `json:"disposition"`
		} `json:"streams"`
		Format struct {
			FormatName string `json:"format_name"`
			BitRate    string `json:"bit_rate"`
			Size       string `json:"size"`
			Duration   string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &p) != nil {
		d.why = "unreadable metadata"
		return d
	}

	// Pick the real video stream — cover art is also codec_type "video", and
	// ffmpeg's 0:v:N counts it too, so streams are mapped by absolute index.
	vi := -1
	var tracks []aTrack
	for i, s := range p.Streams {
		switch {
		case s.CodecType == "video" && s.Disposition.AttachedPic == 0 && vi < 0:
			vi = i
		case s.CodecType == "audio":
			br, _ := strconv.Atoi(s.BitRate)
			tracks = append(tracks, aTrack{idx: s.Index, name: s.CodecName,
				lang: s.Tags.Language, layout: s.ChannelLayout, channels: s.Channels,
				title: s.Tags.Title, def: s.Disposition.Default == 1, br: br,
				copy: s.CodecName == "aac"})
		}
	}
	if vi < 0 {
		d.why = "no video stream"
		return d
	}
	v := p.Streams[vi]
	d.ok, d.vIdx = true, v.Index
	d.isMP4 = strings.Contains(p.Format.FormatName, "mp4")

	d.audios = chooseAudio(tracks)
	d.allAudio = len(d.audios) == len(tracks)
	for i := 0; d.allAudio && i < len(tracks); i++ {
		d.allAudio = d.audios[i].idx == tracks[i].idx
	}

	d.durSec, _ = strconv.ParseFloat(p.Format.Duration, 64)

	// Overall bitrate; derive it from size/duration when the container doesn't
	// carry one. Dropped tracks' weight comes off; re-encoded ones swap theirs
	// for the AAC target.
	overall, _ := strconv.Atoi(p.Format.BitRate)
	if overall <= 0 {
		size, _ := strconv.Atoi(p.Format.Size)
		if d.durSec > 0 && size > 0 {
			overall = int(float64(size) * 8 / d.durSec)
		}
	}
	projected := overall
	for _, t := range tracks {
		kept := false
		for _, k := range d.audios {
			kept = kept || k.idx == t.idx
		}
		switch {
		case !kept && t.br > 0:
			projected -= t.br
		case kept && !t.copy && t.br > 0:
			projected += audioBitrate - t.br
		}
	}
	// Copy-fit codecs, all hw-decode verified on-device with real content
	// (2026-07-20 ladder): 8-bit h264, 8/10-bit HEVC. 10-bit h264 stays out —
	// no hardware decodes Hi10P anywhere.
	d.vCodec = v.CodecName
	hwFit := (v.CodecName == "h264" && v.PixFmt == "yuv420p") ||
		(v.CodecName == "hevc" && (v.PixFmt == "yuv420p" || v.PixFmt == "yuv420p10le"))
	switch {
	case !hwFit:
		d.why = strings.TrimSpace(v.CodecName + " " + v.PixFmt)
	case v.Width > maxWidth || v.Height > maxHeight:
		d.why = fmt.Sprintf("%dx%d", v.Width, v.Height)
	case projected <= 0:
		d.why = "unknown bitrate"
	case projected > maxOverall:
		d.why = fmt.Sprintf("%d kbps", projected/1000)
	default:
		d.vCopy = true
	}

	// Averages hide scene bursts that outrun the link, and containers rarely
	// declare a trustworthy max — so measure it: worst 1-second window over
	// the whole video stream. Only worth it when a copy is on the table.
	if d.vCopy {
		switch peak := peakBitrate(in, v.Index, d.durSec); {
		case peak <= 0:
			d.vCopy, d.why = false, "peak scan failed"
		case peak > maxPeak:
			d.vCopy, d.why = false, fmt.Sprintf("%d kbps peak", peak/1000)
		}
	}

	// A bare stereo downmix plays several dB quieter than the source
	// (swresample scales the mix down to clip-proof it), so each re-encoded
	// track gets a measured static gain toward targetLUFS — a pure volume
	// offset, dynamics untouched. The measure passes are independent
	// decode-only ffmpeg runs, CPU-bound in loudnorm's analysis, so they all
	// run at once — one shared status line, result lines as each pass lands.
	var need []int
	for i := range d.audios {
		if !d.audios[i].copy {
			need = append(need, i)
		}
	}
	if len(need) > 0 {
		track := func(i int) string {
			if len(d.audios) == 1 {
				return ""
			}
			return fmt.Sprintf(" (track %d of %d, %s)", i+1, len(d.audios), d.audios[i].name)
		}
		labels := make([]string, len(need))
		if len(need) == 1 {
			fmt.Printf("▶ Measuring loudness%s…\n", track(need[0]))
		} else {
			for s, i := range need {
				labels[s] = d.audios[i].name
			}
			fmt.Printf("▶ Measuring loudness (%d tracks in parallel)…\n", len(need))
		}
		st := newLoudnessStatus(d.durSec, labels)
		var wg sync.WaitGroup
		for s, i := range need {
			t := &d.audios[i]
			wg.Go(func() {
				t.gainDB, t.hasGain = loudnessGain(in, t.idx, s, st, track(i))
			})
		}
		wg.Wait()
	}
	return d
}

// chooseAudio asks which audio tracks to keep when the input has several —
// numbers in the order they should land in the output (the first kept track
// becomes the output's default). Enter, EOF, or a non-interactive stdin keep
// just the input's default track, matching the old single-track behavior.
func chooseAudio(tracks []aTrack) []aTrack {
	if len(tracks) <= 1 {
		return tracks
	}
	def := 0
	for i, t := range tracks {
		if t.def {
			def = i
			break
		}
	}
	fmt.Printf("▶ %d audio tracks:\n", len(tracks))
	for i, t := range tracks {
		fmt.Printf("  %2d. %s\n", i+1, t.describe())
	}
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Printf("▶ Keep which? (e.g. 1 3 — first kept becomes default; Enter = %d) ", def+1)
		if !sc.Scan() || strings.TrimSpace(sc.Text()) == "" {
			fmt.Printf("▶ Keeping track %d\n", def+1)
			return tracks[def : def+1]
		}
		var sel []aTrack
		seen := map[int]bool{}
		ok := true
		for f := range strings.FieldsSeq(sc.Text()) {
			n, err := strconv.Atoi(f)
			if ok = err == nil && n >= 1 && n <= len(tracks) && !seen[n]; !ok {
				break
			}
			seen[n] = true
			sel = append(sel, tracks[n-1])
		}
		if ok {
			return sel
		}
		fmt.Printf("▶ Track numbers 1-%d, no repeats\n", len(tracks))
	}
}

// describe renders one picker line, e.g. `rus ac3 5.1(side) 448 kbps "Dub"`.
func (t aTrack) describe() string {
	var parts []string
	if t.lang != "" && t.lang != "und" {
		parts = append(parts, t.lang)
	}
	parts = append(parts, t.name)
	switch {
	case t.layout != "":
		parts = append(parts, t.layout)
	case t.channels > 0:
		parts = append(parts, fmt.Sprintf("%dch", t.channels))
	}
	if t.br > 0 {
		parts = append(parts, fmt.Sprintf("%d kbps", t.br/1000))
	}
	if t.title != "" {
		parts = append(parts, `"`+t.title+`"`)
	}
	if t.def {
		parts = append(parts, "(default)")
	}
	return strings.Join(parts, "  ")
}

// peakBitrate returns the video stream's worst 1-second bitrate (bits/s),
// found by bucketing every packet's size into its pts second. Reads the whole
// file (no decoding) — minutes-long inputs take tens of seconds, hence the
// progress line. Returns 0 when the scan fails.
func peakBitrate(in string, idx int, durSec float64) int {
	start := time.Now()
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", strconv.Itoa(idx),
		"-show_entries", "packet=pts_time,size", "-of", "csv=p=0", in)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0
	}
	if err := cmd.Start(); err != nil {
		return 0
	}
	perSec := map[int]int{}
	last := -60.0
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		ptsStr, sizeStr, _ := strings.Cut(sc.Text(), ",")
		pts, err1 := strconv.ParseFloat(ptsStr, 64)
		size, err2 := strconv.Atoi(sizeStr)
		if err1 != nil || err2 != nil { // e.g. pts_time=N/A
			continue
		}
		perSec[int(pts)] += size
		if durSec > 0 && pts >= last+60 { // redraw once per minute of video
			fmt.Printf("\r▶ Measuring peak bitrate… %3.0f%%", min(pts/durSec, 1)*100)
			last = pts
		}
	}
	if cmd.Wait() != nil {
		fmt.Println()
		return 0
	}
	peak := 0
	for _, b := range perSec {
		peak = max(peak, b*8)
	}
	fmt.Printf("\r%-40s\n", fmt.Sprintf("▶ Peak bitrate: %d kbps (1s window) — took %s", peak/1000, took(start)))
	return peak
}

// downmix is the stereo conversion re-encoded audio goes through. As an
// explicit filter (not -ac 2) so the loudness measure pass can run the exact
// signal the encoder will hear — dialogue level shifts with the mix.
const downmix = "aformat=channel_layouts=stereo"

// loudnessGain measures the input audio's integrated loudness (EBU R128, over
// the stereo downmix) and returns the static dB gain that brings it to
// targetLUFS. Peaks the boost pushes over maxTruePeak are the limiter's job —
// capping the gain by whole-file peak instead traded the entire boost for a
// few transients (a real BDRip: −27 LUFS wanting +11 dB, but +1.8 dBTP peaks
// turned the gain into a −3.3 dB *cut*). Decode-bound full pass — tens of
// seconds for a movie. ok=false means the measurement failed; the encode then
// runs plain, as before.
func loudnessGain(in string, idx, slot int, st *loudnessStatus, desc string) (gain float64, ok bool) {
	start := time.Now()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-nostats",
		"-progress", "pipe:1", "-stats_period", "0.5",
		"-i", in, "-map", fmt.Sprintf("0:%d", idx),
		// ebur128, not loudnorm-in-measure-mode: same integrated LUFS, but
		// ~30x faster (loudnorm resamples everything to 192 kHz internally).
		// framelog=verbose keeps the per-100ms lines off stderr; sample peak
		// (log-only — see the whole-file-peak note above) is free, true peak
		// would cost 4x oversampling.
		"-af", downmix+",ebur128=framelog=verbose:peak=sample",
		"-f", "null", os.DevNull)
	var meas strings.Builder
	cmd.Stderr = &meas // ebur128 prints its summary on stderr at stream end
	fail := func() (float64, bool) {
		st.finish(slot, os.Stderr, "▶ Loudness measurement"+desc+" failed — encoding without gain")
		return 0, false
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fail()
	}
	if err := cmd.Start(); err != nil {
		return fail()
	}
	parseProgress(out, func(pos, speed float64) { st.update(slot, pos, speed) })
	if cmd.Wait() != nil {
		return fail()
	}
	lufs, peakDB := math.NaN(), math.NaN()
	for ln := range strings.SplitSeq(meas.String(), "\n") {
		switch f := strings.Fields(ln); {
		case len(f) == 3 && f[0] == "I:" && f[2] == "LUFS":
			lufs, _ = strconv.ParseFloat(f[1], 64)
		case len(f) == 3 && f[0] == "Peak:" && f[2] == "dBFS":
			peakDB, _ = strconv.ParseFloat(f[1], 64)
		}
	}
	if math.IsNaN(lufs) || lufs <= -70 { // -70 = ebur128's silence floor
		return fail()
	}
	gain = targetLUFS - lufs
	st.finish(slot, os.Stdout, fmt.Sprintf("▶ Loudness%s %.1f LUFS (peak %+.1f dBFS) → %+.1f dB gain — took %s",
		desc, lufs, peakDB, gain, took(start)))
	return gain, true
}

// loudnessStatus owns the terminal's status line while concurrent loudness
// passes run: each pass updates its slot, the line redraws with every running
// pass's percent plus the slowest pass's time-left, and finished passes'
// result lines print above it. All terminal writes go through its lock.
type loudnessStatus struct {
	mu     sync.Mutex
	durSec float64
	labels []string  // cell prefix per slot ("" when there's just one pass)
	pos    []float64 // seconds measured; -1 once the pass has finished
	speed  []float64
	width  int // widest line drawn, so \r redraws overwrite cleanly
}

func newLoudnessStatus(durSec float64, labels []string) *loudnessStatus {
	return &loudnessStatus{durSec: durSec, labels: labels,
		pos: make([]float64, len(labels)), speed: make([]float64, len(labels))}
}

func (s *loudnessStatus) update(slot int, pos, speed float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pos[slot], s.speed[slot] = pos, speed
	s.draw()
}

// finish retires the slot's cell and prints line above the status line.
func (s *loudnessStatus) finish(slot int, w io.Writer, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pos[slot] = -1
	fmt.Printf("\r%-*s\r", s.width, "")
	fmt.Fprintln(w, line)
	s.draw()
}

func (s *loudnessStatus) draw() {
	var cells []string
	var left float64
	for i, p := range s.pos {
		if p < 0 {
			continue
		}
		cell := fmtSec(p) + " measured"
		if s.durSec > 0 {
			cell = fmt.Sprintf("%3.0f%%", min(p/s.durSec, 1)*100)
			if l := (s.durSec - p) / s.speed[i]; s.speed[i] > 0 && l >= 0.5 {
				left = max(left, l)
			}
		}
		if s.labels[i] != "" {
			cell = s.labels[i] + " " + cell
		}
		if s.speed[i] > 0 {
			cell += fmt.Sprintf(" (%.1fx)", s.speed[i])
		}
		cells = append(cells, cell)
	}
	if cells == nil {
		return
	}
	line := "▶ " + strings.Join(cells, " · ")
	if left > 0 {
		line += fmt.Sprintf("  ~%s left", fmtSec(left))
	}
	s.width = max(s.width, len(line))
	fmt.Printf("\r%-*s", s.width, line)
}

// audioFilter is the chain a re-encoded track runs through: stereo downmix,
// the measured loudness gain, then a lookahead limiter taming the split-second
// overs (downmix channel summation, boosted transients) that would otherwise
// clip at the encoder; everything under the ceiling passes untouched.
// level=false — its default auto-level would re-normalize and undo the gain;
// latency=true keeps A/V sync across the lookahead delay.
func audioFilter(t aTrack) string {
	f := downmix
	if t.hasGain {
		f += fmt.Sprintf(",volume=%.2fdB", t.gainDB)
	}
	return f + fmt.Sprintf(",alimiter=limit=%.4f:level=false:latency=true", math.Pow(10, maxTruePeak/20))
}

// codecArgs returns the ffmpeg stream-selection and codec flags for a decision.
// aac_at is Apple's AudioToolbox encoder — better than ffmpeg's built-in aac
// at the same bitrate, and always present in macOS ffmpeg builds.
func codecArgs(d decision) []string {
	full := []string{
		"-vf", fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2",
			maxWidth, maxHeight),
		// hevc_videotoolbox: better quality per bit than h264 at the same 8M;
		// projector hw-decodes HEVC 8/10-bit (verified 2026-07). hvc1 tag —
		// players reject mp4's default hev1. No pinned pix_fmt: 8-bit sources
		// stay 8-bit, 10-bit sources encode as Main10 instead of being crushed.
		"-c:v", "hevc_videotoolbox", "-tag:v", "hvc1",
		"-b:v", targetBitrate, "-maxrate", targetBitrate, "-bufsize", targetBufsize,
	}
	if !d.ok { // probe failed: transcode whatever streams are there
		return append(append([]string{"-map", "0:v:0", "-map", "0:a:0?"}, full...),
			"-c:a", "aac_at", "-b:a", strconv.Itoa(audioBitrate), "-af", audioFilter(aTrack{}))
	}
	args := []string{"-map", fmt.Sprintf("0:%d", d.vIdx)}
	for _, t := range d.audios {
		args = append(args, "-map", fmt.Sprintf("0:%d", t.idx))
	}
	if d.vCopy {
		args = append(args, "-c:v", "copy")
		if d.vCodec == "hevc" {
			// copied hevc gets mp4's default hev1 tag, which players reject
			args = append(args, "-tag:v", "hvc1")
		}
	} else {
		args = append(args, full...)
	}
	for n, t := range d.audios {
		if t.copy {
			args = append(args, fmt.Sprintf("-c:a:%d", n), "copy")
		} else {
			args = append(args, fmt.Sprintf("-c:a:%d", n), "aac_at",
				fmt.Sprintf("-b:a:%d", n), strconv.Itoa(audioBitrate),
				fmt.Sprintf("-filter:a:%d", n), audioFilter(t))
		}
		// First kept track becomes the output's default; clear the flag on the
		// rest so players don't inherit a stray default from the source.
		disp := "0"
		if n == 0 {
			disp = "default"
		}
		args = append(args, fmt.Sprintf("-disposition:a:%d", n), disp)
		// mp4 drops the mkv "title" tag; handler_name is what the mov muxer
		// writes (and VLC's track menu shows) — without it same-language
		// tracks are indistinguishable on the projector.
		if t.title != "" {
			args = append(args, fmt.Sprintf("-metadata:s:a:%d", n), "handler_name="+t.title)
		}
	}
	return args
}

func baseName(p string) string {
	b := filepath.Base(p)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

func serveMedia(mod time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(filePath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("contentFeatures.dlna.org",
			"DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")
		http.ServeContent(w, r, "video.mp4", mod, f) // Range/206 for free
	}
}

func serveXML(body func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		w.Write([]byte(body()))
	}
}

// ---- UPnP device + service descriptions ----

func deviceXML() string {
	return `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
 <specVersion><major>1</major><minor>0</minor></specVersion>
 <device>
  <dlna:X_DLNADOC xmlns:dlna="urn:schemas-dlna-org:device-1-0">DMS-1.50</dlna:X_DLNADOC>
  <deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
  <friendlyName>project (Mac)</friendlyName>
  <manufacturer>project</manufacturer>
  <modelName>project</modelName>
  <modelNumber>1</modelNumber>
  <UDN>` + uuid + `</UDN>
  <serviceList>
   <service>
    <serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>
    <serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId>
    <SCPDURL>/ContentDirectory.xml</SCPDURL>
    <controlURL>/ctl/ContentDirectory</controlURL>
    <eventSubURL>/evt/ContentDirectory</eventSubURL>
   </service>
  </serviceList>
 </device>
</root>`
}

func scpdXML() string {
	return `<?xml version="1.0"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
 <specVersion><major>1</major><minor>0</minor></specVersion>
 <actionList>
  <action><name>Browse</name>
   <argumentList>
    <argument><name>ObjectID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable></argument>
    <argument><name>BrowseFlag</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_BrowseFlag</relatedStateVariable></argument>
    <argument><name>Filter</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Filter</relatedStateVariable></argument>
    <argument><name>StartingIndex</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Index</relatedStateVariable></argument>
    <argument><name>RequestedCount</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
    <argument><name>SortCriteria</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SortCriteria</relatedStateVariable></argument>
    <argument><name>Result</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable></argument>
    <argument><name>NumberReturned</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
    <argument><name>TotalMatches</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
    <argument><name>UpdateID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_UpdateID</relatedStateVariable></argument>
   </argumentList>
  </action>
  <action><name>GetSystemUpdateID</name><argumentList><argument><name>Id</name><direction>out</direction><relatedStateVariable>SystemUpdateID</relatedStateVariable></argument></argumentList></action>
  <action><name>GetSortCapabilities</name><argumentList><argument><name>SortCaps</name><direction>out</direction><relatedStateVariable>SortCapabilities</relatedStateVariable></argument></argumentList></action>
  <action><name>GetSearchCapabilities</name><argumentList><argument><name>SearchCaps</name><direction>out</direction><relatedStateVariable>SearchCapabilities</relatedStateVariable></argument></argumentList></action>
 </actionList>
 <serviceStateTable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_ObjectID</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_BrowseFlag</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_Filter</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_Index</name><dataType>ui4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_Count</name><dataType>ui4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_SortCriteria</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_Result</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_UpdateID</name><dataType>ui4</dataType></stateVariable>
  <stateVariable sendEvents="yes"><name>SystemUpdateID</name><dataType>ui4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>SortCapabilities</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>SearchCapabilities</name><dataType>string</dataType></stateVariable>
 </serviceStateTable>
</scpd>`
}

// ---- ContentDirectory SOAP control ----

var reObjectID = regexp.MustCompile(`(?s)<ObjectID>(.*?)</ObjectID>`)
var reFlag = regexp.MustCompile(`(?s)<BrowseFlag>(.*?)</BrowseFlag>`)

func serveControl(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 1<<16)
	n, _ := r.Body.Read(buf)
	body := string(buf[:n])
	action := r.Header.Get("SOAPACTION")

	switch {
	case strings.Contains(action, "Browse"):
		objID := match(reObjectID, body)
		flag := match(reFlag, body)
		var didl string
		var num int
		if flag == "BrowseMetadata" {
			if objID == "0" {
				didl = rootContainer()
			} else {
				didl = videoItem()
			}
			num = 1
		} else { // BrowseDirectChildren
			if objID == "0" {
				didl = videoItem()
				num = 1
			} else {
				didl = didlWrap("")
				num = 0
			}
		}
		soap(w, "Browse", ""+
			"<Result>"+esc(didl)+"</Result>"+
			fmt.Sprintf("<NumberReturned>%d</NumberReturned>", num)+
			fmt.Sprintf("<TotalMatches>%d</TotalMatches>", num)+
			"<UpdateID>1</UpdateID>")
	case strings.Contains(action, "GetSystemUpdateID"):
		soap(w, "GetSystemUpdateID", "<Id>1</Id>")
	case strings.Contains(action, "GetSortCapabilities"):
		soap(w, "GetSortCapabilities", "<SortCaps></SortCaps>")
	case strings.Contains(action, "GetSearchCapabilities"):
		soap(w, "GetSearchCapabilities", "<SearchCaps></SearchCaps>")
	default:
		http.Error(w, "unsupported", 500)
	}
}

func soap(w http.ResponseWriter, action, inner string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	fmt.Fprintf(w, `<?xml version="1.0"?>`+
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">`+
		`<s:Body><u:%sResponse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">%s</u:%sResponse></s:Body></s:Envelope>`,
		action, inner, action)
}

func didlWrap(items string) string {
	return `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">` + items + `</DIDL-Lite>`
}

func rootContainer() string {
	return didlWrap(`<container id="0" parentID="-1" restricted="1" childCount="1">` +
		`<dc:title>project</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`)
}

func videoItem() string {
	res := fmt.Sprintf("http://%s:%s/media", ip, port)
	proto := "http-get:*:video/mp4:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000"
	return didlWrap(`<item id="1" parentID="0" restricted="1">` +
		`<dc:title>` + esc(title) + `</dc:title>` +
		`<upnp:class>object.item.videoItem</upnp:class>` +
		`<res protocolInfo="` + proto + `">` + res + `</res></item>`)
}

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func esc(s string) string { return escaper.Replace(s) }
func match(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// ---- SSDP discovery ----

func ssdp() {
	addr, _ := net.ResolveUDPAddr("udp4", "239.255.255.250:1900")
	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ssdp:", err)
		return
	}
	conn.SetReadBuffer(1 << 20)
	announce("ssdp:alive") // proactively announce on startup

	buf := make([]byte, 2048)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		msg := string(buf[:n])
		if !strings.HasPrefix(msg, "M-SEARCH") {
			continue
		}
		st := header(msg, "ST")
		for _, t := range wanted(st) {
			reply(src, t)
		}
	}
}

// which of our identities to answer for a given search target
func wanted(st string) []string {
	all := []string{
		"upnp:rootdevice",
		uuid,
		"urn:schemas-upnp-org:device:MediaServer:1",
		"urn:schemas-upnp-org:service:ContentDirectory:1",
	}
	if st == "ssdp:all" {
		return all
	}
	for _, t := range all {
		if st == t {
			return []string{t}
		}
	}
	return nil
}

func reply(dst *net.UDPAddr, st string) {
	usn := uuid
	if st != uuid {
		usn = uuid + "::" + st
	}
	msg := "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		"DATE: " + time.Now().UTC().Format(http.TimeFormat) + "\r\n" +
		"EXT:\r\n" +
		"LOCATION: http://" + ip + ":" + port + "/description.xml\r\n" +
		"SERVER: Darwin/1.0 UPnP/1.0 project/1.0\r\n" +
		"ST: " + st + "\r\n" +
		"USN: " + usn + "\r\n\r\n"
	c, err := net.DialUDP("udp4", nil, dst)
	if err != nil {
		return
	}
	defer c.Close()
	c.Write([]byte(msg))
}

func announce(nts string) {
	dst, _ := net.ResolveUDPAddr("udp4", "239.255.255.250:1900")
	c, err := net.DialUDP("udp4", nil, dst)
	if err != nil {
		return
	}
	defer c.Close()
	for _, st := range []string{"upnp:rootdevice", uuid,
		"urn:schemas-upnp-org:device:MediaServer:1",
		"urn:schemas-upnp-org:service:ContentDirectory:1"} {
		usn := uuid
		if st != uuid {
			usn = uuid + "::" + st
		}
		msg := "NOTIFY * HTTP/1.1\r\n" +
			"HOST: 239.255.255.250:1900\r\n" +
			"CACHE-CONTROL: max-age=1800\r\n" +
			"LOCATION: http://" + ip + ":" + port + "/description.xml\r\n" +
			"NT: " + st + "\r\n" +
			"NTS: " + nts + "\r\n" +
			"SERVER: Darwin/1.0 UPnP/1.0 project/1.0\r\n" +
			"USN: " + usn + "\r\n\r\n"
		c.Write([]byte(msg))
	}
}

func header(msg, key string) string {
	for line := range strings.SplitSeq(msg, "\r\n") {
		if i := strings.Index(line, ":"); i > 0 &&
			strings.EqualFold(strings.TrimSpace(line[:i]), key) {
			return strings.Trim(strings.TrimSpace(line[i+1:]), `"`)
		}
	}
	return ""
}

func localIP() string {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}
