package publicmedia

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"
)

// A link to one file of a package is opaque and expires:
//
//	/public/{name}/l/{payload}.{mac}/{basename}
//
// payload is base64url(version | expiry (uint32, Unix seconds) | path inside
// the package). mac is the first 16 bytes of HMAC-SHA256 over the package
// name, its current CID and the payload. A link names the package's CID, so
// a package that is published again (a new CID) ends every link to the old
// one. basename is the file's own name, for logs and a reader of the
// address; it has to agree with the signed path.

const (
	linkVersion = 1
	macBytes    = 16
	// linkStep rounds expiries to whole minutes: every viewer of a minute
	// gets the same links, and the expiry gives away less of the clock.
	linkStep = time.Minute
)

var (
	errLinkMalformed = errors.New("malformed link")
	errLinkSignature = errors.New("link signature mismatch")
	errLinkExpired   = errors.New("link expired")
)

type signer struct {
	key []byte
	ttl time.Duration
}

func (s signer) mac(name, cid string, payload []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("iamfree-public-media/v1\x00"))
	m.Write([]byte(name))
	m.Write([]byte{0})
	m.Write([]byte(cid))
	m.Write([]byte{0})
	m.Write(payload)
	return m.Sum(nil)[:macBytes]
}

// expiry is when a link made now stops working.
func (s signer) expiry(now time.Time) time.Time {
	return now.Add(s.ttl).Truncate(linkStep).Add(linkStep)
}

// token is the {payload}.{mac} part of a link to path, valid until exp.
func (s signer) token(name, cid, path string, exp time.Time) string {
	payload := make([]byte, 5+len(path))
	payload[0] = linkVersion
	binary.BigEndian.PutUint32(payload[1:5], uint32(exp.Unix()))
	copy(payload[5:], path)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(s.mac(name, cid, payload))
}

// open checks a token for the package's current CID and returns the path it
// names and its expiry.
func (s signer) open(name, cid, token string, now time.Time) (string, time.Time, error) {
	encodedPayload, encodedMAC, ok := strings.Cut(token, ".")
	if !ok || len(encodedPayload) > 512 || len(encodedMAC) > 64 {
		return "", time.Time{}, errLinkMalformed
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(encodedPayload)
	if err != nil || len(payload) < 6 || payload[0] != linkVersion {
		return "", time.Time{}, errLinkMalformed
	}
	mac, err := enc.DecodeString(encodedMAC)
	if err != nil || len(mac) != macBytes {
		return "", time.Time{}, errLinkMalformed
	}
	if !hmac.Equal(mac, s.mac(name, cid, payload)) {
		return "", time.Time{}, errLinkSignature
	}
	exp := time.Unix(int64(binary.BigEndian.Uint32(payload[1:5])), 0)
	if !now.Before(exp) {
		return "", exp, errLinkExpired
	}
	path := string(payload[5:])
	if !validPath(path) {
		return "", exp, errLinkMalformed
	}
	return path, exp, nil
}
