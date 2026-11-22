package imageproc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/borg001/ipfs-filestorage/internal/config"
)

// theGHub1/docs#180: one picture must not take the storage container's memory.

// forgedPNG is a valid 1x1 PNG whose header declares another size and depth.
func forgedPNG(t *testing.T, width, height uint32, bitDepth, colorType byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	// IHDR follows the signature: length(4) "IHDR"(4) width(4) height(4) depth(1) colour type(1) ...
	binary.BigEndian.PutUint32(data[16:20], width)
	binary.BigEndian.PutUint32(data[20:24], height)
	data[24], data[25] = bitDepth, colorType
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

func TestDecodeLimitCountsBytesNotOnlyPixels(t *testing.T) {
	cases := []struct {
		name  string
		cfg   image.Config
		allow bool
	}{
		{"8-bit RGBA at the pixel limit", image.Config{Width: 10000, Height: 10000, ColorModel: color.NRGBAModel}, true},
		{"JPEG at the pixel limit", image.Config{Width: 10000, Height: 10000, ColorModel: color.YCbCrModel}, true},
		{"16-bit grey at the pixel limit", image.Config{Width: 10000, Height: 10000, ColorModel: color.Gray16Model}, true},
		{"16-bit RGBA of 49 megapixels", image.Config{Width: 7000, Height: 7000, ColorModel: color.NRGBA64Model}, true},
		{"16-bit RGBA of 64 megapixels", image.Config{Width: 8000, Height: 8000, ColorModel: color.NRGBA64Model}, false},
		{"16-bit RGB of 100 megapixels", image.Config{Width: 10000, Height: 10000, ColorModel: color.RGBA64Model}, false},
		{"over the pixel limit", image.Config{Width: 10001, Height: 10000, ColorModel: color.GrayModel}, false},
		{"sides whose product overflows", image.Config{Width: 1 << 32, Height: 1 << 32, ColorModel: color.GrayModel}, false},
		{"empty", image.Config{Width: 0, Height: 10, ColorModel: color.GrayModel}, false},
	}
	for _, c := range cases {
		err := checkDecodable(c.cfg)
		if c.allow && err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if !c.allow && !errors.Is(err, ErrImageTooLarge) {
			t.Errorf("%s: err = %v, want ErrImageTooLarge", c.name, err)
		}
	}
}

// A 16-bit PNG of 64 megapixels decodes into 512 MB: refused from its header
// on upload and on every delivery preview.
func TestSixteenBitGiantIsRefusedBeforeDecoding(t *testing.T) {
	data := forgedPNG(t, 8000, 8000, 16, 6)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.ColorModel != color.NRGBA64Model {
		t.Fatalf("forged header: %v, %v", cfg.ColorModel, err)
	}
	p := NewProcessor(config.ImageConfig{JPEGQuality: 80}, "")
	if _, err := p.Process(context.Background(), data, "image/png"); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("Process: err = %v, want ErrImageTooLarge", err)
	}
	if _, err := p.ProcessPrivacy(context.Background(), data, "image/png"); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("ProcessPrivacy: err = %v, want ErrImageTooLarge", err)
	}
	size := config.ImageVariant{Width: 100, Height: 100, Mode: "fit"}
	if _, err := p.ConfiguredPreview(context.Background(), data, size); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("ConfiguredPreview: err = %v, want ErrImageTooLarge", err)
	}
	if _, err := p.CroppedPreview(context.Background(), data, size, 0, 0, 0.5, 0.5); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("CroppedPreview: err = %v, want ErrImageTooLarge", err)
	}
}

// Delivery previews decode whole pictures too; they wait for the same slots
// as uploads instead of decoding beside them without a bound.
func TestDeliveryPreviewsWaitForAPictureSlot(t *testing.T) {
	SetProcessSlots(1)
	defer SetProcessSlots(DefaultProcessSlots)
	release, err := acquireSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data := testPNG(t, 64, 48)
	p := NewProcessor(config.ImageConfig{JPEGQuality: 80}, "")
	size := config.ImageVariant{Width: 32, Height: 32, Mode: "fit"}
	preview := map[string]func(context.Context) ([]byte, error){
		"ConfiguredPreview": func(ctx context.Context) ([]byte, error) { return p.ConfiguredPreview(ctx, data, size) },
		"CroppedPreview": func(ctx context.Context) ([]byte, error) {
			return p.CroppedPreview(ctx, data, size, 0.25, 0.25, 0.5, 0.5)
		},
	}
	for name, run := range preview {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, err := run(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s with every slot busy: err = %v, want to wait for a slot", name, err)
		}
	}
	release()
	for name, run := range preview {
		if _, err := run(context.Background()); err != nil {
			t.Errorf("%s with a free slot: %v", name, err)
		}
	}
}

// An avatar frame is a view of the decode: the cut part used to be copied
// at full resolution beside the decode itself.
func TestCroppedPreviewDoesNotCopyTheFrame(t *testing.T) {
	const side = 2000
	data := testPNG(t, side, side)
	p := NewProcessor(config.ImageConfig{JPEGQuality: 80}, "")
	size := config.ImageVariant{Width: 50, Height: 50, Mode: "fit"}
	decoded := uint64(side * side * 4)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	preview, err := p.CroppedPreview(context.Background(), data, size, 0, 0, 1, 1)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > decoded*3/2 {
		t.Fatalf("framed preview allocated %d MB for a %d MB decode, want no full-size copy", allocated>>20, decoded>>20)
	}
	img, err := jpeg.Decode(bytes.NewReader(preview))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 50 || img.Bounds().Dy() != 50 {
		t.Fatalf("preview is %v, want 50x50", img.Bounds())
	}
}

// box is one ISOBMFF box.
func box(kind string, body ...[]byte) []byte {
	content := bytes.Join(body, nil)
	out := make([]byte, 8, 8+len(content))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(content)))
	copy(out[4:8], kind)
	return append(out, content...)
}

func ispe(width, height uint32) []byte {
	body := make([]byte, 12)
	binary.BigEndian.PutUint32(body[4:8], width)
	binary.BigEndian.PutUint32(body[8:12], height)
	return box("ispe", body)
}

// syntheticHEIF is a HEIF header that declares image sizes and holds no
// picture, like a grid of identical tiles a few kilobytes long.
func syntheticHEIF(properties ...[]byte) []byte {
	ftyp := box("ftyp", []byte("heic"), make([]byte, 4), []byte("mif1heic"))
	meta := box("meta", make([]byte, 4), box("hdlr", make([]byte, 24)),
		box("iprp", box("ipco", properties...), box("ipma", make([]byte, 8))))
	return append(ftyp, meta...)
}

func TestHEIFDimensionsAreReadFromTheHeader(t *testing.T) {
	fixture, err := os.ReadFile("testdata/grid-1024x768.heic")
	if err != nil {
		t.Fatal(err)
	}
	if !IsHEIF(fixture) {
		t.Fatal("fixture is not taken for HEIF")
	}
	// A real grid: 4x3 tiles of 256x256, a 64x48 thumbnail, the 1024x768 grid.
	if w, h, ok := heifDimensions(fixture); !ok || w != 1024 || h != 768 {
		t.Fatalf("heifDimensions(grid) = %d, %d, %v, want 1024, 768", w, h, ok)
	}
	if w, h, ok := heifDimensions(syntheticHEIF(ispe(256, 256), ispe(40000, 30000), ispe(64, 48))); !ok || w != 40000 || h != 30000 {
		t.Fatalf("heifDimensions(synthetic) = %d, %d, %v, want the largest declared", w, h, ok)
	}
	if _, _, ok := heifDimensions(syntheticHEIF(box("colr", make([]byte, 4)))); ok {
		t.Fatal("a file without ispe was given a size")
	}
	// Any truncation is parsed without panicking.
	for n := range fixture {
		heifDimensions(fixture[:n])
	}
}

// libheif accepts a gigapixel and heif-convert allocates it outside this
// process's memory limit: the declared size is refused before it runs.
func TestTranscodeHEIFRefusesAGiantBeforeConverting(t *testing.T) {
	p := NewProcessor(config.ImageConfig{JPEGQuality: 80}, "")
	if _, err := p.TranscodeHEIF(context.Background(), syntheticHEIF(ispe(40000, 30000))); !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("err = %v, want ErrImageTooLarge", err)
	}
	if _, err := p.TranscodeHEIF(context.Background(), syntheticHEIF(ispe(1<<32-1, 1<<32-1))); !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("overflowing sides: err = %v, want ErrImageTooLarge", err)
	}
	if _, err := p.TranscodeHEIF(context.Background(), syntheticHEIF()); err == nil {
		t.Fatal("a HEIF without a declared size was converted")
	}
}

func TestTranscodeHEIFStillConvertsAnOrdinaryPhoto(t *testing.T) {
	if _, err := exec.LookPath(heifConvertPath); err != nil {
		t.Skip("heif-convert is not installed")
	}
	fixture, err := os.ReadFile("testdata/grid-1024x768.heic")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProcessor(config.ImageConfig{JPEGQuality: 80}, "")
	converted, err := p.TranscodeHEIF(context.Background(), fixture)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(converted))
	if err != nil || cfg.Width != 1024 || cfg.Height != 768 {
		t.Fatalf("converted = %dx%d, %v; want a 1024x768 JPEG", cfg.Width, cfg.Height, err)
	}
}
