// project: fit ONE video file for the projector, serve it over HTTP (with
// Range), and advertise it as a minimal UPnP/DLNA MediaServer so VLC's
// "Local Network" browser discovers it. No external daemon, stdlib only.
//
// usage: project <file>
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const uuid = "uuid:6f3a2b10-0000-4a00-8000-project0dlna1" // stable across runs

// Fixed "fit" target: the projector is native 1080p. 7M is proven over its
// 5 GHz Wi-Fi; 8M is on trial. A file already within these limits is copied,
// not re-encoded. maxOverall must stay above targetBitrate + audioBitrate,
// or fit files would be re-encoded UP.
const (
	targetBitrate = "8M"      // video bitrate when a full transcode is needed
	targetBufsize = "16M"     // decoder buffer: 2× targetBitrate
	audioBitrate  = 256_000   // bits/s; AAC target when audio is re-encoded
	maxOverall    = 8_500_000 // bits/s; inputs at/under this are considered "fits"
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
// H.264 + AAC); an input that already fits in an mp4 container is served as-is.
func fit(in string) string {
	out := filepath.Join(filepath.Dir(in), baseName(in)+"-ready.mp4")

	// Reuse an existing -ready.mp4 if it's newer than the source — but probe it
	// first, so leftovers from older versions of this tool (which allowed other
	// bitrates) can't smuggle an unfit file past the fixed target.
	if o, err := os.Stat(out); err == nil {
		if i, err2 := os.Stat(in); err2 == nil && o.ModTime().After(i.ModTime()) {
			if isFit(plan(out)) {
				fmt.Printf("▶ Reusing %s (delete it to force a re-encode)\n", filepath.Base(out))
				return out
			}
			fmt.Printf("▶ Existing %s doesn't fit the current target — redoing it\n", filepath.Base(out))
		}
	}

	d := plan(in)
	if isFit(d) {
		fmt.Printf("▶ Already fit — serving %s as-is\n", filepath.Base(in))
		return in
	}
	switch {
	case !d.ok || !d.vCopy:
		fmt.Printf("▶ Transcoding (%s) → %s\n", d.why, filepath.Base(out))
	case d.aIdx >= 0 && !d.aCopy:
		fmt.Printf("▶ Video is fine — re-encoding audio only (audio %s) → %s\n", d.aName, filepath.Base(out))
	default:
		fmt.Printf("▶ Already fit — remuxing (lossless) → %s\n", filepath.Base(out))
	}

	// Encode to a .part file and rename on success, so an interrupted/failed run
	// never leaves a half-encoded <name>-ready.mp4 for the cache to reuse.
	tmp := out + ".part"
	err := ffmpeg(in, tmp, codecArgs(d))
	if err != nil && d.ok && (d.vCopy || d.aCopy) {
		// Streams that look fit can still refuse to copy into mp4 (bad
		// timestamps, packed bitstreams); re-encoding regenerates them.
		fmt.Fprintln(os.Stderr, "▶ Copy failed — retrying as a full transcode")
		err = ffmpeg(in, tmp, codecArgs(decision{ok: true, vIdx: d.vIdx, aIdx: d.aIdx}))
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
// all: already an mp4 whose video (and audio, if any) would be plain copies.
func isFit(d decision) bool {
	return d.ok && d.isMP4 && d.vCopy && (d.aIdx < 0 || d.aCopy)
}

func ffmpeg(in, tmp string, codec []string) error {
	argv := append([]string{"-hide_banner", "-loglevel", "error", "-nostats",
		"-y", "-i", in}, codec...)
	argv = append(argv, "-movflags", "+faststart", "-f", "mp4", tmp)
	cmd := exec.Command("ffmpeg", argv...)
	cmd.Stderr = os.Stderr // ffmpeg is quiet now; only real errors reach here
	return cmd.Run()
}

// decision is plan()'s verdict: which streams to use and how little work each
// needs. The zero value (ok=false) means "probe failed, assume the worst".
type decision struct {
	ok           bool
	vIdx, aIdx   int    // absolute stream indices; aIdx < 0 → no audio
	vCopy, aCopy bool   // stream already fits the target → plain copy
	isMP4        bool   // container is already mp4/mov
	aName        string // audio codec name, for messages
	why          string // short reason when the video needs a transcode
}

// plan probes the input with ffprobe and decides, per stream, whether a copy
// suffices. Video fits if it's 8-bit 4:2:0 h264 within 1080p and the projected
// output bitrate stays under maxOverall; audio fits if it's already aac. Any
// probe failure falls back to a full transcode (safe default).
func plan(in string) decision {
	d := decision{aIdx: -1}
	out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", in).Output()
	if err != nil {
		d.why = "probe failed"
		return d
	}
	var p struct {
		Streams []struct {
			Index       int    `json:"index"`
			CodecType   string `json:"codec_type"`
			CodecName   string `json:"codec_name"`
			PixFmt      string `json:"pix_fmt"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			BitRate     string `json:"bit_rate"`
			MaxBitRate  string `json:"max_bit_rate"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
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
	vi, ai := -1, -1
	for i, s := range p.Streams {
		if s.CodecType == "video" && s.Disposition.AttachedPic == 0 && vi < 0 {
			vi = i
		}
		if s.CodecType == "audio" && ai < 0 {
			ai = i
		}
	}
	if vi < 0 {
		d.why = "no video stream"
		return d
	}
	v := p.Streams[vi]
	d.ok, d.vIdx = true, v.Index
	d.isMP4 = strings.Contains(p.Format.FormatName, "mp4")

	abr := 0
	if ai >= 0 {
		d.aIdx = p.Streams[ai].Index
		d.aName = p.Streams[ai].CodecName
		d.aCopy = d.aName == "aac"
		abr, _ = strconv.Atoi(p.Streams[ai].BitRate)
	}

	// Overall bitrate; derive it from size/duration when the container doesn't
	// carry one. Re-encoded audio swaps its weight for the AAC target.
	overall, _ := strconv.Atoi(p.Format.BitRate)
	if overall <= 0 {
		size, _ := strconv.Atoi(p.Format.Size)
		if dur, _ := strconv.ParseFloat(p.Format.Duration, 64); dur > 0 && size > 0 {
			overall = int(float64(size) * 8 / dur)
		}
	}
	projected := overall
	if ai >= 0 && !d.aCopy && abr > 0 {
		projected = overall - abr + audioBitrate
	}
	peak, _ := strconv.Atoi(v.MaxBitRate)

	switch {
	case v.CodecName != "h264" || v.PixFmt != "yuv420p":
		d.why = strings.TrimSpace(v.CodecName + " " + v.PixFmt)
	case v.Width > maxWidth || v.Height > maxHeight:
		d.why = fmt.Sprintf("%dx%d", v.Width, v.Height)
	case projected <= 0:
		d.why = "unknown bitrate"
	case projected > maxOverall:
		d.why = fmt.Sprintf("%d kbps", projected/1000)
	case peak > 2*maxOverall:
		d.why = fmt.Sprintf("%d kbps peaks", peak/1000)
	default:
		d.vCopy = true
	}
	return d
}

// aac_at is Apple's AudioToolbox encoder — better than ffmpeg's built-in aac
// at the same bitrate, and always present in macOS ffmpeg builds.
var audioArgs = []string{"-c:a", "aac_at", "-b:a", strconv.Itoa(audioBitrate), "-ac", "2"}

// codecArgs returns the ffmpeg stream-selection and codec flags for a decision.
func codecArgs(d decision) []string {
	full := []string{
		"-vf", fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2",
			maxWidth, maxHeight),
		"-c:v", "h264_videotoolbox", "-profile:v", "high",
		"-b:v", targetBitrate, "-maxrate", targetBitrate, "-bufsize", targetBufsize,
	}
	if !d.ok { // probe failed: transcode whatever streams are there
		return append(append([]string{"-map", "0:v:0", "-map", "0:a:0?"}, full...), audioArgs...)
	}
	args := []string{"-map", fmt.Sprintf("0:%d", d.vIdx)}
	if d.aIdx >= 0 {
		args = append(args, "-map", fmt.Sprintf("0:%d", d.aIdx))
	}
	if d.vCopy {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, full...)
	}
	if d.aIdx >= 0 {
		if d.aCopy {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, audioArgs...)
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
