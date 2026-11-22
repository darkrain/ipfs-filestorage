// Package publicmedia serves public media packages: films for the platform's
// own public sites (the landings), with no account and no per-file policy.
//
// A package is a directory of an HLS film added to IPFS as one CID (so it is
// pinned and copied like every other file). The configuration names each
// package and the sites allowed to play it. Only /public/{name}/master.m3u8
// and /public/{name}/poster.jpg are opened by name; every other file - the
// variant playlists, init segments, media segments, subtitles - is reached
// only through the opaque expiring links the playlists are rewritten to.
//
// The rest of the storage is unchanged: these routes stand in front of the
// authenticated router and answer nothing but /public/, and nothing outside a
// configured package's directory can be named through them.
package publicmedia

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
)

// Prefix is the only address space of this package's routes.
const Prefix = "/public/"

const (
	maxPlaylistBytes = 1 << 20
	minKeyBytes      = 32
	// Cached playlists of the configured packages. They are immutable (a CID)
	// and small; a film has a master and a handful of variants and subtitles.
	maxCachedPlaylists = 256
)

// Fetcher reads a file inside a UnixFS directory CID.
type Fetcher interface {
	ClusterTryFetchPath(ctx context.Context, cid string, filePath string) (io.ReadCloser, error)
}

// Config is the public media configuration, as read from the environment.
type Config struct {
	// Packages maps a package name to its directory CID.
	Packages map[string]string
	// Origins are the sites (scheme://host[:port]) allowed to play packages.
	Origins []string
	// LinkTTL is how long a link in a rewritten playlist stays valid.
	LinkTTL time.Duration
	// SigningKey signs the links: base64 (standard or URL) of at least 32 bytes.
	SigningKey string
	// AllowNoOrigin accepts a request that names no site at all: no Origin,
	// no Referer and no Sec-Fetch-Site. Safari's native HLS player sends such
	// requests for playlists and segments; so does any command-line client.
	AllowNoOrigin bool
}

var packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type pkg struct {
	name string
	cid  string
}

// Handler serves /public/.
type Handler struct {
	packages map[string]pkg
	origins  map[string]bool
	signer   signer
	fetch    Fetcher
	allowNo  bool
	now      func() time.Time

	mu        sync.Mutex
	playlists map[string][]byte
}

// New checks cfg and returns the handler, or nil when no package is
// configured: the feature is off and /public/ stays with the main router.
func New(cfg Config, fetch Fetcher) (*Handler, error) {
	if len(cfg.Packages) == 0 {
		return nil, nil
	}
	if fetch == nil {
		return nil, errors.New("public media: no IPFS reader")
	}
	h := &Handler{
		packages:  map[string]pkg{},
		origins:   map[string]bool{},
		fetch:     fetch,
		allowNo:   cfg.AllowNoOrigin,
		now:       time.Now,
		playlists: map[string][]byte{},
	}
	for name, value := range cfg.Packages {
		if !packageName.MatchString(name) {
			return nil, fmt.Errorf("public media: invalid package name %q", name)
		}
		parsed, err := cid.Decode(value)
		if err != nil {
			return nil, fmt.Errorf("public media: package %q: invalid CID: %w", name, err)
		}
		h.packages[name] = pkg{name: name, cid: parsed.String()}
	}
	for _, origin := range cfg.Origins {
		normalized, ok := normalizeOrigin(origin)
		if !ok {
			return nil, fmt.Errorf("public media: invalid site origin %q", origin)
		}
		h.origins[normalized] = true
	}
	if len(h.origins) == 0 {
		return nil, errors.New("public media: PUBLIC_MEDIA_ORIGINS is empty")
	}
	key, err := decodeKey(cfg.SigningKey)
	if err != nil {
		return nil, err
	}
	ttl := cfg.LinkTTL
	if ttl < time.Minute || ttl > 6*time.Hour {
		return nil, fmt.Errorf("public media: link TTL %s outside 1m..6h", ttl)
	}
	h.signer = signer{key: key, ttl: ttl}
	return h, nil
}

func decodeKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(value); err == nil {
			if len(key) < minKeyBytes {
				return nil, fmt.Errorf("public media: signing key has %d bytes, at least %d required", len(key), minKeyBytes)
			}
			return key, nil
		}
	}
	return nil, errors.New("public media: PUBLIC_MEDIA_SIGNING_KEY must be base64 of at least 32 random bytes")
}

// normalizeOrigin returns scheme://host[:port] in lower case for an https
// origin, or for http only on a loopback host (a developer's machine).
func normalizeOrigin(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	if scheme != "https" && !(scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return "", false
	}
	return scheme + "://" + host, true
}

// Mount answers /public/ itself and passes every other request to next.
func (h *Handler) Mount(next http.Handler, wrap ...func(http.Handler) http.Handler) http.Handler {
	var public http.Handler = h
	for i := len(wrap) - 1; i >= 0; i-- {
		public = wrap[i](public)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, Prefix) {
			public.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Names lists the configured packages (for the start-up log).
func (h *Handler) Names() []string {
	names := make([]string, 0, len(h.packages))
	for name, p := range h.packages {
		names = append(names, name+"="+p.cid)
	}
	return names
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	header.Add("Vary", "Origin")

	origin, allowed := h.site(r)
	if origin != "" && allowed {
		header.Set("Access-Control-Allow-Origin", origin)
		header.Set("Access-Control-Expose-Headers", "Content-Length, Content-Type")
	}
	if !allowed {
		h.fail(w, http.StatusForbidden, "This film plays only on the platform's sites")
		return
	}
	switch r.Method {
	case http.MethodOptions:
		header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		header.Set("Access-Control-Allow-Headers", "Range")
		header.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet, http.MethodHead:
	default:
		header.Set("Allow", "GET, HEAD, OPTIONS")
		h.fail(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	// /public/{name}/master.m3u8, /public/{name}/poster.jpg,
	// /public/{name}/l/{token}/{basename}
	rest := strings.TrimPrefix(r.URL.Path, Prefix)
	parts := strings.Split(rest, "/")
	p, ok := h.packages[parts[0]]
	if !ok {
		h.fail(w, http.StatusNotFound, "Not found")
		return
	}
	switch {
	case len(parts) == 2 && parts[1] == entryPlaylist:
		h.servePlaylist(w, r, p, entryPlaylist)
	case len(parts) == 2 && parts[1] == posterFile:
		h.serveFile(w, r, p, posterFile, time.Time{})
	case len(parts) == 4 && parts[1] == "l":
		file, exp, err := h.signer.open(p.name, p.cid, parts[2], h.now())
		if err == nil && path.Base(file) != parts[3] {
			err = errLinkMalformed
		}
		if err != nil {
			if errors.Is(err, errLinkExpired) {
				h.fail(w, http.StatusForbidden, "Link expired")
			} else {
				h.fail(w, http.StatusForbidden, "Invalid link")
			}
			return
		}
		if isPlaylist(file) {
			h.servePlaylist(w, r, p, file)
		} else {
			h.serveFile(w, r, p, file, exp)
		}
	default:
		h.fail(w, http.StatusNotFound, "Not found")
	}
}

// site says which allowed site a request comes from. A request is refused when
// it names a site that is not allowed, when it is a top-level navigation (the
// address opened in a tab), or when it is cross-site without saying from
// where. One that names no site at all is accepted only with AllowNoOrigin.
func (h *Handler) site(r *http.Request) (string, bool) {
	if mode := r.Header.Get("Sec-Fetch-Mode"); strings.EqualFold(mode, "navigate") {
		return "", false
	}
	if dest := strings.ToLower(r.Header.Get("Sec-Fetch-Dest")); dest == "document" || dest == "iframe" || dest == "frame" || dest == "embed" || dest == "object" {
		return "", false
	}
	if raw := r.Header.Get("Origin"); raw != "" {
		origin, ok := normalizeOrigin(raw)
		if ok && h.origins[origin] {
			return origin, true
		}
		return "", false
	}
	if raw := r.Header.Get("Referer"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			return "", false
		}
		origin, ok := normalizeOrigin(u.Scheme + "://" + u.Host)
		return "", ok && h.origins[origin]
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && !strings.EqualFold(site, "same-origin") {
		return "", false
	}
	return "", h.allowNo
}

func (h *Handler) link(p pkg, file string, exp time.Time) string {
	return Prefix + p.name + "/l/" + h.signer.token(p.name, p.cid, file, exp) + "/" + path.Base(file)
}

func (h *Handler) servePlaylist(w http.ResponseWriter, r *http.Request, p pkg, file string) {
	body, err := h.playlist(r.Context(), p, file)
	if err != nil {
		log.Printf("[PUBLIC] %s/%s: %v", p.name, file, err)
		h.fail(w, http.StatusNotFound, "Not found")
		return
	}
	exp := h.signer.expiry(h.now())
	out, err := rewritePlaylist(body, path.Dir(file), func(child string) string { return h.link(p, child, exp) })
	if err != nil {
		log.Printf("[PUBLIC] %s/%s: %v", p.name, file, err)
		h.fail(w, http.StatusBadGateway, "Package playlist is invalid")
		return
	}
	header := w.Header()
	header.Set("Content-Type", contentType(file))
	header.Set("Content-Disposition", "inline")
	// The links in it expire; a stored copy would hand out dead links.
	header.Set("Cache-Control", "private, no-store")
	header.Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(out)
	}
}

func (h *Handler) playlist(ctx context.Context, p pkg, file string) ([]byte, error) {
	key := p.cid + "/" + file
	h.mu.Lock()
	cached, ok := h.playlists[key]
	h.mu.Unlock()
	if ok {
		return cached, nil
	}
	reader, err := h.fetch.ClusterTryFetchPath(ctx, p.cid, file)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	body, err := io.ReadAll(io.LimitReader(reader, maxPlaylistBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPlaylistBytes {
		return nil, errors.New("playlist too large")
	}
	h.mu.Lock()
	if len(h.playlists) >= maxCachedPlaylists {
		clear(h.playlists)
	}
	h.playlists[key] = body
	h.mu.Unlock()
	return body, nil
}

// serveFile streams one file of the package. exp is the link's expiry; zero
// for the poster, which is not signed.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, p pkg, file string, exp time.Time) {
	reader, err := h.fetch.ClusterTryFetchPath(r.Context(), p.cid, file)
	if err != nil {
		h.fail(w, http.StatusNotFound, "Not found")
		return
	}
	defer reader.Close()
	header := w.Header()
	header.Set("Content-Type", contentType(file))
	header.Set("Content-Disposition", "inline")
	if exp.IsZero() {
		header.Set("Cache-Control", "public, max-age=3600")
	} else {
		// The bytes never change (a CID); the browser keeps them no longer than
		// the link lives, and no shared cache keeps them at all.
		remaining := int(exp.Sub(h.now()) / time.Second)
		if remaining < 0 {
			remaining = 0
		}
		header.Set("Cache-Control", "private, max-age="+strconv.Itoa(min(remaining, 3600)))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, reader); err != nil && r.Context().Err() == nil {
		log.Printf("[PUBLIC] %s/%s: stream interrupted: %v", p.name, file, err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, message string) {
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	header.Del("Content-Disposition")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
