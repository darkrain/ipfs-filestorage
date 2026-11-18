package mediagrant

// Keep this file identical in API and file storage, like grant.go.
//
// Two more sealed envelopes use the media grant key ring. Each has its own
// associated data, so a grant, a media session and an upload receipt can
// never be opened as one another.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// MediaSessionTTL bounds a media session. A media address that leaks - a
// copied picture link, a browser history entry, an access log - reads media
// as its owner for at most this long and opens nothing else: the session
// token inside it is never accepted outside GET /file and /stream.
const MediaSessionTTL = time.Hour

// ReceiptTTL is how long an upload receipt may wait before the API records
// it.
const ReceiptTTL = 24 * time.Hour

const (
	purposeMediaSession = "iamfree-media-session:v1:"
	purposeReceipt      = "iamfree-upload-receipt:v1:"
	maxReceiptLength    = 256 << 10
)

// MediaSession carries the viewer's session to the storage inside an
// encrypted, short-lived envelope, so the token itself is never written into
// a media address.
type MediaSession struct {
	Version   int    `json:"v"`
	Token     string `json:"t"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// Receipt is the storage's word that it produced these files for this
// session: an upload (Kind "file") or the output of the video encoder (Kind
// "video", CID is the master playlist and Children everything the encoder
// wrote with it - playlists, segments and posters).
type Receipt struct {
	Version   int      `json:"v"`
	Session   string   `json:"s"`
	Kind      string   `json:"k"`
	CID       string   `json:"c"`
	Children  []string `json:"x,omitempty"`
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
}

func (c *Codec) purposeAAD(purpose, id string) []byte {
	return []byte(purpose + c.audience + ":" + id)
}

func (c *Codec) sealPurpose(purpose string, value interface{}) (string, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	aead := c.keys[c.active]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plain, c.purposeAAD(purpose, c.active))
	return c.active + "." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *Codec) openPurpose(purpose, encoded string, limit int, value interface{}) error {
	if encoded == "" || len(encoded) > limit {
		return ErrInvalid
	}
	id, body, ok := strings.Cut(encoded, ".")
	if !ok {
		return ErrInvalid
	}
	aead := c.keys[id]
	if aead == nil {
		return ErrInvalid
	}
	sealed, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead() {
		return ErrInvalid
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], c.purposeAAD(purpose, id))
	if err != nil || json.Unmarshal(plain, value) != nil {
		return ErrInvalid
	}
	return nil
}

// SealMediaSession wraps a session token for media addresses.
func (c *Codec) SealMediaSession(token string, now time.Time) (string, time.Time, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 1024 {
		return "", time.Time{}, ErrInvalid
	}
	expires := now.Add(MediaSessionTTL)
	encoded, err := c.sealPurpose(purposeMediaSession, MediaSession{Version: 1, Token: token, IssuedAt: now.Unix(), ExpiresAt: expires.Unix()})
	return encoded, expires, err
}

// OpenMediaSession returns the session token a media session carries.
func (c *Codec) OpenMediaSession(encoded string, now time.Time) (string, error) {
	var session MediaSession
	if err := c.openPurpose(purposeMediaSession, encoded, 4096, &session); err != nil {
		return "", err
	}
	if session.Version != 1 || strings.TrimSpace(session.Token) == "" || session.IssuedAt <= 0 ||
		session.IssuedAt > now.Unix()+60 || session.ExpiresAt <= session.IssuedAt ||
		session.ExpiresAt-session.IssuedAt > int64(MediaSessionTTL/time.Second) {
		return "", ErrInvalid
	}
	if now.Unix() >= session.ExpiresAt {
		return "", ErrExpired
	}
	return session.Token, nil
}

// SealReceipt issues a receipt for files the storage has just produced for
// the session that uploaded them.
func (c *Codec) SealReceipt(token, kind, cid string, children []string, now time.Time) (string, error) {
	if strings.TrimSpace(token) == "" || (kind != "file" && kind != "video") || !receiptCID(cid) {
		return "", ErrInvalid
	}
	for _, child := range children {
		if !receiptCID(child) {
			return "", ErrInvalid
		}
	}
	return c.sealPurpose(purposeReceipt, Receipt{Version: 1, Session: Session(strings.TrimSpace(token)), Kind: kind, CID: cid, Children: children, IssuedAt: now.Unix(), ExpiresAt: now.Add(ReceiptTTL).Unix()})
}

// OpenReceipt reads a receipt issued to this session.
func (c *Codec) OpenReceipt(encoded, token string, now time.Time) (Receipt, error) {
	var receipt Receipt
	token = strings.TrimSpace(token)
	if token == "" {
		return Receipt{}, ErrInvalid
	}
	if err := c.openPurpose(purposeReceipt, encoded, maxReceiptLength, &receipt); err != nil {
		return Receipt{}, err
	}
	if subtle.ConstantTimeCompare([]byte(receipt.Session), []byte(Session(token))) != 1 {
		return Receipt{}, ErrInvalid
	}
	if receipt.Version != 1 || (receipt.Kind != "file" && receipt.Kind != "video") || !receiptCID(receipt.CID) ||
		receipt.IssuedAt <= 0 || receipt.IssuedAt > now.Unix()+60 || receipt.ExpiresAt <= receipt.IssuedAt ||
		receipt.ExpiresAt-receipt.IssuedAt > int64(ReceiptTTL/time.Second) {
		return Receipt{}, ErrInvalid
	}
	for _, child := range receipt.Children {
		if !receiptCID(child) {
			return Receipt{}, ErrInvalid
		}
	}
	if now.Unix() >= receipt.ExpiresAt {
		return Receipt{}, ErrExpired
	}
	return receipt, nil
}

func receiptCID(cid string) bool {
	return cid != "" && len(cid) <= 256 && !strings.ContainsAny(cid, "/\\?#: \t\r\n\"'")
}

// PolicyCallerTTL bounds a storage's proof that it is the one asking the
// media policy.
const PolicyCallerTTL = 5 * time.Minute

const purposePolicyCaller = "iamfree-media-policy-caller:v1:"

// PolicyCallerHeader carries that proof. The policy answers a storage with
// the locators it needs to serve a blurred picture; a reader asking the same
// endpoint gets the decision without them.
const PolicyCallerHeader = "X-Media-Policy-Caller"

type policyCaller struct {
	Version   int   `json:"v"`
	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
}

// SealPolicyCaller proves the bearer holds the media grant keys.
func (c *Codec) SealPolicyCaller(now time.Time) (string, error) {
	return c.sealPurpose(purposePolicyCaller, policyCaller{Version: 1, IssuedAt: now.Unix(), ExpiresAt: now.Add(PolicyCallerTTL).Unix()})
}

// OpenPolicyCaller checks such a proof.
func (c *Codec) OpenPolicyCaller(encoded string, now time.Time) error {
	var caller policyCaller
	if err := c.openPurpose(purposePolicyCaller, encoded, 1024, &caller); err != nil {
		return err
	}
	if caller.Version != 1 || caller.IssuedAt <= 0 || caller.IssuedAt > now.Unix()+60 || caller.ExpiresAt <= caller.IssuedAt ||
		caller.ExpiresAt-caller.IssuedAt > int64(PolicyCallerTTL/time.Second) {
		return ErrInvalid
	}
	if now.Unix() >= caller.ExpiresAt {
		return ErrExpired
	}
	return nil
}
