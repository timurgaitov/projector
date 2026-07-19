// serve-one: serve ONE video file over HTTP (with Range), and advertise it as a
// minimal UPnP/DLNA MediaServer so VLC's "Local Network" browser discovers it.
// No external daemon, stdlib only.
//
// usage: serve-one <file> [port]
package main

import (
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

var (
	filePath string
	title    string
	port     string
	ip       string
)

func main() {
	// usage: project [-raw] <file> [bitrate, e.g. 8M]
	args := os.Args[1:]
	raw := false
	if len(args) > 0 && (args[0] == "-raw" || args[0] == "--raw") {
		raw, args = true, args[1:]
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: project [-raw] <file> [bitrate, e.g. 8M]")
		os.Exit(1)
	}
	input := args[0]
	bitrate := "6M"
	if len(args) > 1 {
		bitrate = args[1]
	}
	port = "1111"
	title = baseName(input)

	if raw {
		filePath = input // serve as-is, no transcode
	} else {
		filePath = transcode(input, bitrate)
	}
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

// transcode fits any input to a fixed projector/wifi profile (1080p cap,
// H.264 High, AAC stereo, faststart) via ffmpeg, returning the output path.
func transcode(in, br string) string {
	out := filepath.Join(filepath.Dir(in), baseName(in)+"-ready.mp4")

	// Reuse an existing -ready.mp4 if it's newer than the source (skip re-encode).
	if o, err := os.Stat(out); err == nil {
		if i, err2 := os.Stat(in); err2 == nil && o.ModTime().After(i.ModTime()) {
			fmt.Printf("▶ Reusing %s (already fit — delete it to force re-encode)\n", filepath.Base(out))
			return out
		}
	}

	fmt.Printf("▶ Fitting %q at %s (1080p / H.264 High / AAC) → %s\n", in, br, filepath.Base(out))
	// Encode to a .part file and rename on success, so an interrupted/failed
	// run never leaves a half-encoded <name>-ready.mp4 for the cache to reuse.
	tmp := out + ".part"
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats",
		"-y", "-i", in,
		"-map", "0:v:0", "-map", "0:a:0",
		"-vf", "scale='min(1920,iw)':-2",
		"-c:v", "h264_videotoolbox", "-profile:v", "high",
		"-b:v", br, "-maxrate", br, "-bufsize", bufsize(br),
		"-c:a", "aac", "-b:a", "160k", "-ac", "2",
		"-movflags", "+faststart", "-f", "mp4", tmp)
	cmd.Stderr = os.Stderr // ffmpeg is quiet now; only real errors reach here
	if err := cmd.Run(); err != nil {
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

// bufsize returns 2× the bitrate (e.g. "6M" -> "12M"), leaving it unchanged
// if it isn't in the simple "<n>M" form.
func bufsize(br string) string {
	if n, err := strconv.Atoi(strings.TrimSuffix(br, "M")); err == nil && strings.HasSuffix(br, "M") {
		return strconv.Itoa(n*2) + "M"
	}
	return br
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

func esc(s string) string  { return escaper.Replace(s) }
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
