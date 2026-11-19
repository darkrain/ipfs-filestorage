package mediagrant

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Keep this file identical in API and file storage.

func TestMediaSessionCarriesTheSessionSealed(t *testing.T) {
	c := testCodec(t, "stand-a")
	now := time.Unix(1_800_000_000, 0)
	encoded, expires, err := c.SealMediaSession(" session-one ", now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "session-one") {
		t.Fatal("the media session shows the session token")
	}
	if expires.Sub(now) != MediaSessionTTL {
		t.Fatalf("expires in %s", expires.Sub(now))
	}
	token, err := c.OpenMediaSession(encoded, now.Add(MediaSessionTTL-time.Second))
	if err != nil || token != "session-one" {
		t.Fatalf("opened %q, %v", token, err)
	}
	if _, err := c.OpenMediaSession(encoded, now.Add(MediaSessionTTL)); !errors.Is(err, ErrExpired) {
		t.Fatalf("an expired media session opened: %v", err)
	}
	if _, err := testCodec(t, "stand-b").OpenMediaSession(encoded, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("another deployment opened the media session: %v", err)
	}
}

// A grant, a media session and a receipt are sealed with one key ring, and
// none of them opens as another.
func TestEnvelopesDoNotOpenAsOneAnother(t *testing.T) {
	c := testCodec(t, "stand-a")
	claim := testClaims()
	now := time.Unix(claim.IssuedAt, 0)
	grant, err := c.Seal(claim, now)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := c.SealMediaSession("session-one", now)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.SealReceipt("session-one", "file", "bafyfile", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.OpenMediaSession(grant, now); err == nil {
		t.Fatal("a grant opened as a media session")
	}
	if _, err := c.OpenMediaSession(receipt, now); err == nil {
		t.Fatal("a receipt opened as a media session")
	}
	if _, err := c.OpenReceipt(session, "session-one", now); err == nil {
		t.Fatal("a media session opened as a receipt")
	}
	if _, err := c.Open(session, "session-one", now); err == nil {
		t.Fatal("a media session opened as a grant")
	}
}

func TestReceiptIsBoundToItsSession(t *testing.T) {
	c := testCodec(t, "stand-a")
	now := time.Unix(1_800_000_000, 0)
	encoded, err := c.SealReceipt("session-one", "video", "bafymaster", []string{"bafyplaylist", "bafyposter"}, now)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.OpenReceipt(encoded, "session-one", now.Add(time.Hour))
	if err != nil || receipt.Kind != "video" || receipt.CID != "bafymaster" || len(receipt.Children) != 2 {
		t.Fatalf("receipt %+v, %v", receipt, err)
	}
	if _, err := c.OpenReceipt(encoded, "session-two", now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("another session opened the receipt: %v", err)
	}
	if _, err := c.OpenReceipt(encoded, "session-one", now.Add(ReceiptTTL)); !errors.Is(err, ErrExpired) {
		t.Fatalf("an expired receipt opened: %v", err)
	}
	if _, err := c.SealReceipt("session-one", "video", "bafy/../x", nil, now); err == nil {
		t.Fatal("a receipt was sealed for a path")
	}
}

func TestPolicyCallerProofIsSealedAndShortLived(t *testing.T) {
	c := testCodec(t, "stand-a")
	now := time.Unix(1_800_000_000, 0)
	proof, err := c.SealPolicyCaller(now)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.OpenPolicyCaller(proof, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.OpenPolicyCaller(proof, now.Add(PolicyCallerTTL)); !errors.Is(err, ErrExpired) {
		t.Fatalf("an old proof opened: %v", err)
	}
	if err := testCodec(t, "stand-b").OpenPolicyCaller(proof, now); err == nil {
		t.Fatal("another deployment's proof opened")
	}
	session, _, _ := c.SealMediaSession("session-one", now)
	if err := c.OpenPolicyCaller(session, now); err == nil {
		t.Fatal("a media session opened as a policy caller")
	}
}
