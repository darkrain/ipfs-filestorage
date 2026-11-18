package imageproc

import (
	"context"
	"image"
	"math"
	"sync"
	"sync/atomic"

	xdraw "golang.org/x/image/draw"
)

// Image work holds several full-size copies of a picture at once: the
// decoded source, its normalized copy, a blurred one per pass, the encoding
// for ffmpeg. /upload-multiple ran one such job per file - up to twenty at
// once - and a HEIC conversion beside each, so one batch or two large photos
// took the whole storage container down, and with it every picture being
// read (theGHub1/docs#180). Every upload's picture work in this process now
// waits for one of a few slots.

// DefaultProcessSlots is how many pictures one process works on at once.
const DefaultProcessSlots = 1

var (
	slotsMu sync.Mutex
	slots   = make(chan struct{}, DefaultProcessSlots)
	// slotsBusy and slotsPeak count the work in progress, for tests.
	slotsBusy atomic.Int64
	slotsPeak atomic.Int64
)

// SetProcessSlots sets how many pictures the process works on at once.
func SetProcessSlots(n int) {
	if n < 1 {
		n = 1
	}
	slotsMu.Lock()
	defer slotsMu.Unlock()
	if cap(slots) != n {
		slots = make(chan struct{}, n)
	}
}

func acquireSlot(ctx context.Context) (func(), error) {
	slotsMu.Lock()
	current := slots
	slotsMu.Unlock()
	select {
	case current <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	busy := slotsBusy.Add(1)
	for {
		peak := slotsPeak.Load()
		if busy <= peak || slotsPeak.CompareAndSwap(peak, busy) {
			break
		}
	}
	return func() {
		slotsBusy.Add(-1)
		<-current
	}, nil
}

// DefaultWorkingPixels bounds the copy of a picture that privacy variants
// and sizes are made from: twelve megapixels, a phone photo. The blurred
// variants never need more, and the largest size served is far smaller.
const DefaultWorkingPixels = 12_000_000

// workingImage is src scaled down to at most maxPixels, or src itself when
// it already fits. The caller drops its reference to the full decode, so a
// hundred-megapixel upload is held whole only for the moment of scaling.
func workingImage(src image.Image, maxPixels int) image.Image {
	if maxPixels <= 0 {
		maxPixels = DefaultWorkingPixels
	}
	bounds := src.Bounds()
	pixels := float64(bounds.Dx()) * float64(bounds.Dy())
	if pixels <= float64(maxPixels) {
		return src
	}
	scale := math.Sqrt(float64(maxPixels) / pixels)
	width := max(1, int(math.Floor(float64(bounds.Dx())*scale)))
	height := max(1, int(math.Floor(float64(bounds.Dy())*scale)))
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, bounds, xdraw.Src, nil)
	return dst
}

// ProcessSlotPeak is the most pictures worked on at once since the last
// ResetProcessSlotPeak.
func ProcessSlotPeak() int64 { return slotsPeak.Load() }

// ResetProcessSlotPeak starts counting the peak again.
func ResetProcessSlotPeak() { slotsPeak.Store(slotsBusy.Load()) }
