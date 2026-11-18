package imageproc

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/borg001/ipfs-filestorage/internal/config"
)

type recordingFaceDetector struct {
	mu     sync.Mutex
	pixels int
	delay  time.Duration
}

func (d *recordingFaceDetector) Detect(src image.Image) ([]image.Rectangle, error) {
	time.Sleep(d.delay)
	d.mu.Lock()
	defer d.mu.Unlock()
	if pixels := src.Bounds().Dx() * src.Bounds().Dy(); pixels > d.pixels {
		d.pixels = pixels
	}
	return nil, nil
}

func boundedTestProcessor(detector faceDetector, workingPixels int) *Processor {
	privacy := config.DefaultImagePrivacyConfig()
	privacy.BlurRadius = 2
	privacy.WorkingPixels = workingPixels
	return &Processor{
		cfg: config.ImageConfig{
			ProcessingEnabled: true,
			Variants:          []config.ImageVariant{{Key: "100x100", Width: 100, Height: 100}},
			OutputFormat:      "jpeg",
			JPEGQuality:       80,
			ResizePolicy:      "cover-center",
			Privacy:           privacy,
		},
		faceDetector: detector,
	}
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y += 7 {
		for x := 0; x < width; x += 7 {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The blurred variants of a large upload are made from a bounded copy, not
// from the picture at full size: blurring a hundred-megapixel photo held
// about 1.5 GiB and the storage container had 1536 MiB (theGHub1/docs#180).
func TestPrivacyVariantsWorkOnABoundedCopy(t *testing.T) {
	const limit = 40_000
	detector := &recordingFaceDetector{}
	result, err := boundedTestProcessor(detector, limit).Process(context.Background(), testPNG(t, 1000, 800), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if result.Width != 1000 || result.Height != 800 {
		t.Fatalf("the original is described as %dx%d, want 1000x800", result.Width, result.Height)
	}
	if detector.pixels == 0 || detector.pixels > limit {
		t.Fatalf("faces were looked for in %d pixels, want at most %d", detector.pixels, limit)
	}
	seen := 0
	for _, variant := range result.Variants {
		if variant.Key != config.PrivacyBlurVariantKey && variant.Key != config.PrivacyFaceBlurVariantKey {
			continue
		}
		seen++
		if variant.Width*variant.Height > limit {
			t.Fatalf("%s was made at %dx%d, above %d pixels", variant.Key, variant.Width, variant.Height, limit)
		}
		decoded, _, err := image.DecodeConfig(bytes.NewReader(variant.Data))
		if err != nil || decoded.Width*decoded.Height > limit {
			t.Fatalf("%s holds %dx%d (%v)", variant.Key, decoded.Width, decoded.Height, err)
		}
	}
	if seen != 2 {
		t.Fatalf("privacy variants: %d, want 2", seen)
	}
}

// Pictures sent at once - an /upload-multiple batch, two uploads, HEIC
// conversions beside them - are worked on a few at a time across the process.
func TestProcessRunsWithinTheProcessSlots(t *testing.T) {
	SetProcessSlots(1)
	ResetProcessSlotPeak()
	processor := boundedTestProcessor(&recordingFaceDetector{delay: 30 * time.Millisecond}, 40_000)
	data := testPNG(t, 300, 200)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := processor.Process(context.Background(), data, "image/png"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak := ProcessSlotPeak(); peak != 1 {
		t.Fatalf("%d pictures were worked on at once, want 1", peak)
	}
}

// A request that gives up while waiting for a slot does not take one.
func TestProcessSlotWaitEndsWithTheRequest(t *testing.T) {
	SetProcessSlots(1)
	release, err := acquireSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := boundedTestProcessor(&recordingFaceDetector{}, 40_000).Process(ctx, testPNG(t, 50, 50), "image/png"); err == nil {
		t.Fatal("a picture was worked on while the only slot was taken")
	}
}
