package imageproc

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// heifConvertPath is the libheif command line tool. It is used the same way
// jpegtran and ffmpeg are: the format work stays in the tool that owns it.
const heifConvertPath = "heif-convert"

// heifBrands are the ISOBMFF brands of a still image an ordinary browser
// cannot display. A phone shooting in "High Efficiency" hands over exactly
// these.
var heifBrands = map[string]bool{
	"heic": true, "heix": true, "heim": true, "heis": true,
	"hevc": true, "hevx": true, "hevm": true, "hevs": true,
	"mif1": true, "msf1": true,
}

// IsHEIF reports whether the payload is a HEIF/HEIC still image. The answer is
// read from the file itself: a browser reports the type inconsistently and Go's
// own content sniffing does not know the format at all.
func IsHEIF(data []byte) bool {
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return false
	}
	return heifBrands[strings.ToLower(string(data[8:12]))]
}

// JPEGFilename renames a converted source so its stored name says what it now
// is.
func JPEGFilename(filename string) string {
	base := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	if strings.TrimSpace(base) == "" {
		base = "image"
	}
	return base + ".jpg"
}

// TranscodeHEIF turns a HEIF/HEIC still into a JPEG. It runs once, at ingest,
// so the stored original, every variant and the delivery link all carry a
// format every browser can display.
func (p *Processor) TranscodeHEIF(ctx context.Context, data []byte) ([]byte, error) {
	// heif-convert decodes the whole picture in its own memory, beside this
	// process and outside GOMEMLIMIT, and libheif itself allows a gigapixel.
	// A grid of identical tiles is a few kilobytes declaring that much, so
	// the declared size is checked before the tool runs (theGHub1/docs#180).
	width, height, ok := heifDimensions(data)
	if !ok {
		return nil, fmt.Errorf("heif: image size is not declared")
	}
	if err := checkDecodable(image.Config{Width: width, Height: height, ColorModel: color.NRGBAModel}); err != nil {
		return nil, err
	}
	release, err := acquireSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	dir, err := os.MkdirTemp("", "heif")
	if err != nil {
		return nil, fmt.Errorf("heif workspace: %w", err)
	}
	defer os.RemoveAll(dir)

	source := filepath.Join(dir, "source.heic")
	target := filepath.Join(dir, "source.jpg")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		return nil, fmt.Errorf("heif source: %w", err)
	}

	quality := p.cfg.JPEGQuality
	if quality < 1 || quality > 100 {
		quality = 90
	}
	cmd := exec.CommandContext(ctx, heifConvertPath, "-q", strconv.Itoa(quality), source, target)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("heif-convert failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	converted, err := os.ReadFile(target)
	if err != nil || len(converted) == 0 {
		// A file holding several images is written as source-1.jpg and so on.
		// The first image of such a source is the photo that was uploaded.
		matches, globErr := filepath.Glob(filepath.Join(dir, "source-*.jpg"))
		if globErr != nil || len(matches) == 0 {
			return nil, fmt.Errorf("heif-convert produced no image")
		}
		converted, err = os.ReadFile(matches[0])
		if err != nil || len(converted) == 0 {
			return nil, fmt.Errorf("heif-convert produced no image")
		}
	}
	return converted, nil
}

// heifDimensions is the largest image size a HEIF file declares: the widest
// "ispe" (image spatial extents) property in meta/iprp/ipco. Every image item
// must carry one (ISO/IEC 23008-12); a grid's own ispe is the full picture,
// its tiles and thumbnails are smaller.
func heifDimensions(data []byte) (width, height int, ok bool) {
	meta, found := heifChild(data, "meta")
	if !found || len(meta) < 4 {
		return 0, 0, false
	}
	iprp, found := heifChild(meta[4:], "iprp") // meta is a full box
	if !found {
		return 0, 0, false
	}
	ipco, found := heifChild(iprp, "ipco")
	if !found {
		return 0, 0, false
	}
	var best uint64
	heifEach(ipco, func(kind string, body []byte) {
		// ispe: version and flags (4), image_width (4), image_height (4).
		if kind != "ispe" || len(body) < 12 {
			return
		}
		w := binary.BigEndian.Uint32(body[4:8])
		h := binary.BigEndian.Uint32(body[8:12])
		if pixels := uint64(w) * uint64(h); !ok || pixels > best {
			best, width, height, ok = pixels, int(w), int(h), true
		}
	})
	return width, height, ok
}

// heifChild is the body of the first box of the given type at this level.
func heifChild(data []byte, want string) ([]byte, bool) {
	var found []byte
	var ok bool
	heifEach(data, func(kind string, body []byte) {
		if !ok && kind == want {
			found, ok = body, true
		}
	})
	return found, ok
}

// heifEach walks the ISOBMFF boxes at one level and stops at the first box
// whose size does not fit.
func heifEach(data []byte, visit func(kind string, body []byte)) {
	for len(data) >= 8 {
		size := uint64(binary.BigEndian.Uint32(data[0:4]))
		kind := string(data[4:8])
		header := uint64(8)
		switch size {
		case 0: // to the end of the enclosing box
			size = uint64(len(data))
		case 1: // 64-bit size follows the type
			if len(data) < 16 {
				return
			}
			size, header = binary.BigEndian.Uint64(data[8:16]), 16
		}
		if size < header || size > uint64(len(data)) {
			return
		}
		visit(kind, data[header:size])
		data = data[size:]
	}
}
