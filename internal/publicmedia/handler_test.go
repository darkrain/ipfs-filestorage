package publicmedia

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	testCID   = "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi"
	otherCID  = "bafybeihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
	siteA     = "https://iamfree24.com"
	siteB     = "https://iamtopmember.com"
	foreign   = "https://example.org"
	testKey64 = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes
)

const master = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Русский",LANGUAGE="ru",DEFAULT=NO,AUTOSELECT=YES,URI="subs/ru.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=640x360,CODECS="avc1.64001e,mp4a.40.2",SUBTITLES="subs"
360p/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3300000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2",SUBTITLES="subs"
720p/index.m3u8
`

const variant = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.000000,
seg_0000.m4s
#EXTINF:2.500000,
seg_0001.m4s
#EXT-X-ENDLIST
`

type fakeFetcher struct {
	files map[string]string
	reads int
}

func (f *fakeFetcher) ClusterTryFetchPath(_ context.Context, cid, p string) (io.ReadCloser, error) {
	f.reads++
	body, ok := f.files[cid+"/"+p]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func testPackage() *fakeFetcher {
	files := map[string]string{
		"master.m3u8":         master,
		"360p/index.m3u8":     variant,
		"360p/init.mp4":       "init-360",
		"360p/seg_0000.m4s":   "segment-360-0",
		"360p/seg_0001.m4s":   "segment-360-1",
		"720p/index.m3u8":     variant,
		"720p/init.mp4":       "init-720",
		"720p/seg_0000.m4s":   "segment-720-0",
		"subs/ru.m3u8":        "#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6.5,\n../subs.ru.vtt\n#EXT-X-ENDLIST\n",
		"subs.ru.vtt":         "WEBVTT\n\n00:00.000 --> 00:02.000\nПривет\n",
		"poster.jpg":          "jpeg",
		"secret/notes.txt":    "never",
		"360p/../master.m3u8": "never",
	}
	f := &fakeFetcher{files: map[string]string{}}
	for k, v := range files {
		f.files[testCID+"/"+k] = v
	}
	return f
}

func newTestHandler(t *testing.T, f Fetcher, mutate ...func(*Config)) *Handler {
	t.Helper()
	cfg := Config{
		Packages:      map[string]string{"first": testCID},
		Origins:       []string{siteA, siteB},
		LinkTTL:       45 * time.Minute,
		SigningKey:    testKey64,
		AllowNoOrigin: true,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	h, err := New(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var fromA = map[string]string{"Origin": siteA, "Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}

var linkLine = regexp.MustCompile(`/public/first/l/[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/[A-Za-z0-9_.-]+`)

func links(body string) []string { return linkLine.FindAllString(body, -1) }

func TestMasterIsRewrittenToSignedLinks(t *testing.T) {
	h := newTestHandler(t, testPackage())
	w := get(h, "/public/first/master.m3u8", fromA)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Fatalf("content type %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != siteA {
		t.Fatalf("ACAO %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("cache %q", got)
	}
	body := w.Body.String()
	if strings.Contains(body, "360p/index.m3u8") || strings.Contains(body, "subs/ru.m3u8") {
		t.Fatalf("package paths leaked:\n%s", body)
	}
	if n := len(links(body)); n != 3 {
		t.Fatalf("want 3 links (2 variants, 1 subtitle), got %d:\n%s", n, body)
	}
	if !strings.Contains(body, `SUBTITLES="subs"`) || !strings.Contains(body, `NAME="Русский"`) {
		t.Fatalf("tags lost:\n%s", body)
	}
}

// Walks master → variant → init/segment and master → subtitles → vtt.
func TestLinksReachEveryFileOfTheFilm(t *testing.T) {
	h := newTestHandler(t, testPackage())
	m := links(get(h, "/public/first/master.m3u8", fromA).Body.String())
	v := get(h, m[1], fromA) // 360p, after the subtitle playlist
	if v.Code != http.StatusOK {
		t.Fatalf("variant %d: %s", v.Code, v.Body)
	}
	children := links(v.Body.String())
	if len(children) != 3 || !strings.HasSuffix(children[0], "/init.mp4") {
		t.Fatalf("variant links %v\n%s", children, v.Body)
	}
	init := get(h, children[0], fromA)
	if init.Body.String() != "init-360" || init.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("init %q %q", init.Body, init.Header().Get("Content-Type"))
	}
	if cc := init.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "private, max-age=") {
		t.Fatalf("segment cache %q", cc)
	}
	if cd := init.Header().Get("Content-Disposition"); cd != "inline" {
		t.Fatalf("disposition %q", cd)
	}
	if seg := get(h, children[2], fromA); seg.Body.String() != "segment-360-1" {
		t.Fatalf("segment %q", seg.Body)
	}
	subs := get(h, m[0], fromA)
	vtt := links(subs.Body.String())
	if len(vtt) != 1 {
		t.Fatalf("subtitle playlist %s", subs.Body)
	}
	cue := get(h, vtt[0], fromA)
	if cue.Header().Get("Content-Type") != "text/vtt; charset=utf-8" || !strings.Contains(cue.Body.String(), "Привет") {
		t.Fatalf("vtt %q %q", cue.Header().Get("Content-Type"), cue.Body)
	}
}

func TestOriginPolicy(t *testing.T) {
	h := newTestHandler(t, testPackage())
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"allowed origin", fromA, 200},
		{"second allowed origin (cross-site)", map[string]string{"Origin": siteB, "Sec-Fetch-Site": "cross-site"}, 200},
		{"foreign origin", map[string]string{"Origin": foreign, "Sec-Fetch-Site": "cross-site"}, 403},
		{"opaque origin", map[string]string{"Origin": "null"}, 403},
		{"origin lookalike", map[string]string{"Origin": "https://iamfree24.com.example.org"}, 403},
		{"http downgrade of an allowed host", map[string]string{"Origin": "http://iamfree24.com"}, 403},
		{"allowed referer, no origin (Safari native HLS)", map[string]string{"Referer": siteA + "/ru/"}, 200},
		{"foreign referer", map[string]string{"Referer": foreign + "/page"}, 403},
		{"no site at all (AVFoundation, curl)", nil, 200},
		{"cross-site without origin or referer", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors"}, 403},
		{"opened in a tab", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 403},
		{"navigation from our own page", map[string]string{"Referer": siteA + "/ru/", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 403},
		{"framed", map[string]string{"Referer": siteA + "/", "Sec-Fetch-Dest": "iframe"}, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := get(h, "/public/first/master.m3u8", c.headers)
			if w.Code != c.want {
				t.Fatalf("status %d, want %d: %s", w.Code, c.want, w.Body)
			}
			if c.want == 403 && w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("CORS granted on a refusal")
			}
		})
	}
	strict := newTestHandler(t, testPackage(), func(c *Config) { c.AllowNoOrigin = false })
	if w := get(strict, "/public/first/master.m3u8", nil); w.Code != 403 {
		t.Fatalf("AllowNoOrigin=false let a request without a site through: %d", w.Code)
	}
}

func TestSegmentLinkFromAnotherSiteIsRefused(t *testing.T) {
	h := newTestHandler(t, testPackage())
	m := links(get(h, "/public/first/master.m3u8", fromA).Body.String())
	seg := links(get(h, m[1], fromA).Body.String())[1]
	if w := get(h, seg, map[string]string{"Origin": foreign}); w.Code != 403 {
		t.Fatalf("foreign origin got the segment: %d", w.Code)
	}
	if w := get(h, seg, map[string]string{"Referer": foreign + "/"}); w.Code != 403 {
		t.Fatalf("foreign referer got the segment: %d", w.Code)
	}
}

func TestExpiredAndForgedLinks(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	h := newTestHandler(t, testPackage())
	h.now = func() time.Time { return now }
	m := links(get(h, "/public/first/master.m3u8", fromA).Body.String())
	variantLink := m[1]
	if w := get(h, variantLink, fromA); w.Code != 200 {
		t.Fatalf("fresh link %d", w.Code)
	}
	h.now = func() time.Time { return now.Add(47 * time.Minute) }
	w := get(h, variantLink, fromA)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "expired") {
		t.Fatalf("expired link: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != siteA {
		t.Fatal("a refusal to an allowed site must stay readable to its player (it reloads the master)")
	}
	h.now = func() time.Time { return now }

	token := strings.Split(variantLink, "/")[4]
	payload, mac, _ := strings.Cut(token, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	tampered := append([]byte{}, raw...)
	tampered[4]++ // a later expiry
	forged := []string{
		"/public/first/l/" + base64.RawURLEncoding.EncodeToString(tampered) + "." + mac + "/index.m3u8",
		"/public/first/l/" + base64.RawURLEncoding.EncodeToString(bytes.Replace(raw, []byte("360p"), []byte("720p"), 1)) + "." + mac + "/index.m3u8",
		"/public/first/l/" + token + "/master.m3u8", // basename does not match
		"/public/first/l/" + payload + "/index.m3u8",
		"/public/first/l/garbage.garbage/index.m3u8",
	}
	for _, f := range forged {
		if w := get(h, f, fromA); w.Code != 403 {
			t.Fatalf("forged %s: %d", f, w.Code)
		}
	}
}

func TestRepublishedPackageEndsOldLinks(t *testing.T) {
	f := testPackage()
	for k, v := range f.files {
		f.files[strings.Replace(k, testCID, otherCID, 1)] = v
	}
	old := newTestHandler(t, f)
	link := links(get(old, "/public/first/master.m3u8", fromA).Body.String())[1]
	renewed := newTestHandler(t, f, func(c *Config) { c.Packages = map[string]string{"first": otherCID} })
	if w := get(renewed, link, fromA); w.Code != 403 {
		t.Fatalf("link to the previous CID still works: %d", w.Code)
	}
}

func TestOnlyPackageFilesByLinkOrEntry(t *testing.T) {
	h := newTestHandler(t, testPackage())
	for _, target := range []string{
		"/public/first/360p/index.m3u8",
		"/public/first/360p/seg_0000.m4s",
		"/public/first/secret/notes.txt",
		"/public/first/",
		"/public/first",
		"/public/",
		"/public/other/master.m3u8",
		"/public/first/../first/master.m3u8",
		"/public/first/l/",
	} {
		if w := get(h, target, fromA); w.Code != 404 && w.Code != 403 {
			t.Fatalf("%s: %d", target, w.Code)
		}
	}
	poster := get(h, "/public/first/poster.jpg", fromA)
	if poster.Code != 200 || poster.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("poster %d %q", poster.Code, poster.Header().Get("Content-Type"))
	}
}

func TestPlaylistsMayNameOnlyTheirOwnPackage(t *testing.T) {
	for _, bad := range []string{
		"#EXTM3U\nhttps://evil.example/seg.m4s\n",
		"#EXTM3U\n/etc/passwd.m4s\n",
		"#EXTM3U\n../../outside.m4s\n",
		"#EXTM3U\n#EXT-X-MAP:URI=\"https://evil.example/init.mp4\"\n",
		"#EXTM3U\nseg.m4s?x=1\n",
		"#EXTM3U\nnotes.txt\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n",
		"not a playlist",
	} {
		if _, err := rewritePlaylist([]byte(bad), "360p", func(p string) string { return p }); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	out, err := rewritePlaylist([]byte("\xef\xbb\xbf#EXTM3U\r\n#EXT-X-MAP:URI=\"init.mp4\"\r\nseg_0000.m4s\r\n"), "360p", func(p string) string { return "<" + p + ">" })
	if err != nil || string(out) != "#EXTM3U\n#EXT-X-MAP:URI=\"<360p/init.mp4>\"\n<360p/seg_0000.m4s>\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestPreflightAndMethods(t *testing.T) {
	h := newTestHandler(t, testPackage())
	r := httptest.NewRequest(http.MethodOptions, "/public/first/master.m3u8", nil)
	r.Header.Set("Origin", siteB)
	r.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != siteB {
		t.Fatalf("preflight %d %q", w.Code, w.Header().Get("Access-Control-Allow-Origin"))
	}
	r = httptest.NewRequest(http.MethodOptions, "/public/first/master.m3u8", nil)
	r.Header.Set("Origin", foreign)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign preflight %d", w.Code)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		r = httptest.NewRequest(method, "/public/first/master.m3u8", strings.NewReader("x"))
		r.Header.Set("Origin", siteA)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
}

func TestPlaylistsAreReadOnceFromIPFS(t *testing.T) {
	f := testPackage()
	h := newTestHandler(t, f)
	get(h, "/public/first/master.m3u8", fromA)
	get(h, "/public/first/master.m3u8", fromA)
	if f.reads != 1 {
		t.Fatalf("master read %d times", f.reads)
	}
}

func TestMountKeepsOtherRoutesWithTheMainRouter(t *testing.T) {
	h := newTestHandler(t, testPackage())
	main := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	mounted := h.Mount(main)
	for _, p := range []string{"/file/" + testCID, "/stream/" + testCID, "/publicity", "/config"} {
		if w := get(mounted, p, nil); w.Code != http.StatusTeapot {
			t.Fatalf("%s did not reach the main router: %d", p, w.Code)
		}
	}
	if w := get(mounted, "/public/first/master.m3u8", fromA); w.Code != 200 {
		t.Fatalf("public route %d", w.Code)
	}
}

func TestConfigValidation(t *testing.T) {
	if h, err := New(Config{}, testPackage()); h != nil || err != nil {
		t.Fatal("no packages must leave the feature off")
	}
	bad := []func(*Config){
		func(c *Config) { c.Packages = map[string]string{"First": testCID} },
		func(c *Config) { c.Packages = map[string]string{"../x": testCID} },
		func(c *Config) { c.Packages = map[string]string{"first": "not-a-cid"} },
		func(c *Config) { c.Packages = map[string]string{"first": ""} },
		func(c *Config) { c.Origins = nil },
		func(c *Config) { c.Origins = []string{"http://iamfree24.com"} },
		func(c *Config) { c.Origins = []string{"https://iamfree24.com/path"} },
		func(c *Config) { c.Origins = []string{"*"} },
		func(c *Config) { c.SigningKey = "" },
		func(c *Config) { c.SigningKey = base64.StdEncoding.EncodeToString([]byte("short")) },
		func(c *Config) { c.LinkTTL = 10 * time.Second },
		func(c *Config) { c.LinkTTL = 24 * time.Hour },
	}
	for i, mutate := range bad {
		cfg := Config{Packages: map[string]string{"first": testCID}, Origins: []string{siteA}, LinkTTL: time.Hour, SigningKey: testKey64}
		mutate(&cfg)
		if _, err := New(cfg, testPackage()); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestPathValidation(t *testing.T) {
	for _, ok := range []string{"master.m3u8", "360p/seg_0001.m4s", "subs/ru.m3u8", "subs.ru.vtt", "poster.jpg"} {
		if !validPath(ok) {
			t.Fatalf("refused %q", ok)
		}
	}
	for _, bad := range []string{"", "/master.m3u8", "../x.m4s", "a/../b.m4s", "a//b.m4s", ".hidden.m4s", "a/b/c/d/e.m4s", "notes.txt", "x.html", "a\\b.m4s", "seg .m4s", "a/./b.m4s"} {
		if validPath(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}
