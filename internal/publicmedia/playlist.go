package publicmedia

import (
	"bufio"
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Files a package may hand out, by extension. Anything else in the package
// directory is never served.
var contentTypes = map[string]string{
	".m3u8": "application/vnd.apple.mpegurl",
	".mp4":  "video/mp4",
	".m4s":  "video/mp4",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".ts":   "video/mp2t",
	".vtt":  "text/vtt; charset=utf-8",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".png":  "image/png",
}

const (
	// entryPlaylist is the one file of a package opened without a signed link.
	entryPlaylist = "master.m3u8"
	// posterFile is the package's still picture; it is no part of the film.
	posterFile = "poster.jpg"
	maxPathLen = 200
	maxDepth   = 4
)

var pathSegment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}$`)

// validPath accepts a clean relative path of plain names with a served
// extension: no "..", no leading "/", no empty or hidden segments.
func validPath(p string) bool {
	if p == "" || len(p) > maxPathLen || path.Clean(p) != p || strings.HasPrefix(p, "/") {
		return false
	}
	segments := strings.Split(p, "/")
	if len(segments) > maxDepth {
		return false
	}
	for _, s := range segments {
		if !pathSegment.MatchString(s) || strings.Contains(s, "..") {
			return false
		}
	}
	_, ok := contentTypes[strings.ToLower(path.Ext(p))]
	return ok
}

func contentType(p string) string {
	return contentTypes[strings.ToLower(path.Ext(p))]
}

func isPlaylist(p string) bool {
	return strings.EqualFold(path.Ext(p), ".m3u8")
}

// resolve turns a URI written in the playlist at dir into a path inside the
// package. Absolute addresses, queries and anything that leaves the package
// are refused: a package names only its own files.
func resolve(dir, uri string) (string, error) {
	if uri == "" || strings.Contains(uri, "://") || strings.HasPrefix(uri, "/") || strings.ContainsAny(uri, "?#\\") {
		return "", fmt.Errorf("playlist URI %q is not a relative package path", uri)
	}
	joined := path.Join(dir, uri)
	if !validPath(joined) {
		return "", fmt.Errorf("playlist URI %q leaves the package or has an unserved type", uri)
	}
	return joined, nil
}

var uriAttribute = regexp.MustCompile(`URI="([^"]*)"`)

// rewritePlaylist replaces every URI of an HLS playlist - the URI lines and
// the URI="…" attributes of tags such as EXT-X-MAP and EXT-X-MEDIA - with the
// link that link gives for it. A playlist at dir names files relative to dir.
func rewritePlaylist(body []byte, dir string, link func(p string) string) ([]byte, error) {
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	if !bytes.HasPrefix(body, []byte("#EXTM3U")) {
		return nil, fmt.Errorf("not an HLS playlist")
	}
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), maxPlaylistBytes)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#"):
			// Encryption keys and session data are no part of a public film.
			if strings.HasPrefix(trimmed, "#EXT-X-KEY") || strings.HasPrefix(trimmed, "#EXT-X-SESSION-KEY") || strings.HasPrefix(trimmed, "#EXT-X-SESSION-DATA") {
				return nil, fmt.Errorf("unsupported tag in a public playlist")
			}
			var failure error
			line = uriAttribute.ReplaceAllStringFunc(line, func(attribute string) string {
				uri := uriAttribute.FindStringSubmatch(attribute)[1]
				p, err := resolve(dir, uri)
				if err != nil {
					failure = err
					return attribute
				}
				return `URI="` + link(p) + `"`
			})
			if failure != nil {
				return nil, failure
			}
		default:
			p, err := resolve(dir, trimmed)
			if err != nil {
				return nil, err
			}
			line = link(p)
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
