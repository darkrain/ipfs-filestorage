package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/borg001/ipfs-filestorage/internal/middleware"
)

// uploadBudget bounds what one session uploads in a day on this instance.
// Storage does not know the account behind a session, only its token, and
// uploads were unmetered: any signed-in session could fill the disk. A nil
// budget allows everything.
type uploadBudget struct {
	mu    sync.Mutex
	limit int64
	day   string
	used  map[string]int64
}

func newUploadBudget(limit int64) *uploadBudget {
	if limit <= 0 {
		return nil
	}
	return &uploadBudget{limit: limit, used: map[string]int64{}}
}

// allow charges an upload to its session. A static API key is a service, not
// a session, and is not metered; a request whose session cannot be named is
// refused rather than let through unmetered (theGHub1/docs#170).
func (b *uploadBudget) allow(r *http.Request, size int64) bool {
	if b == nil {
		return true
	}
	if middleware.IsAPIKey(r.Context()) {
		return true
	}
	return b.take(uploadSessionKey(r), size)
}

// take records size bytes for the session and answers whether they fit.
func (b *uploadBudget) take(key string, size int64) bool {
	if b == nil {
		return true
	}
	if key == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	today := time.Now().UTC().Format("2006-01-02")
	if b.day != today {
		b.day, b.used = today, map[string]int64{}
	}
	if b.used[key]+size > b.limit {
		return false
	}
	b.used[key] += size
	return true
}

// uploadSessionKey names the session an upload comes from without keeping
// its token. It is the token authentication accepted - resolved once by the
// auth middleware, in its order and trimmed - never a second reading of the
// request: the budget used to hash the untrimmed header and the first ?token=
// while authentication took ?access_token=, a trimmed header and the last
// ?token=, so each spelling of one session got a budget of its own.
func uploadSessionKey(r *http.Request) string {
	token := middleware.SessionToken(r.Context())
	if token == "" {
		if scheme, value, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " "); ok && strings.EqualFold(scheme, "Bearer") {
			token = strings.TrimSpace(value)
		}
	}
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

// transcodeSlots is how many videos this instance cuts into HLS segments at
// once. Every upload started its own ffmpeg with no limit, so a few long
// videos took both cores and everything else waited. The browser now
// compresses the video and ffmpeg only copies its streams (theGHub1/docs#201).
var transcodeSlots = make(chan struct{}, 2)

// errTranscodeBusy is an upload that waited too long for a slot.
var errTranscodeBusy = errors.New("transcoding is busy")

const (
	transcodeQueueWait = 2 * time.Minute
	transcodeTimeout   = 3 * time.Minute
)

// acquireTranscodeSlot waits for a free slot, as long as the upload is alive
// and not longer than transcodeQueueWait.
func acquireTranscodeSlot(ctx context.Context) (func(), error) {
	timer := time.NewTimer(transcodeQueueWait)
	defer timer.Stop()
	select {
	case transcodeSlots <- struct{}{}:
		return func() { <-transcodeSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errTranscodeBusy
	}
}
