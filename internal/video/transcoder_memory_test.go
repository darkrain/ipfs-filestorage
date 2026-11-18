package video

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/borg001/ipfs-filestorage/internal/config"
)

// Every rendition was encoded at the source's full size by three x264
// encoders at once, each with threads and a look-ahead sized by the host's
// cores. A 1920x1080 recording from the browser (verification video,
// theGHub1/docs#150) needed about 2.5 GiB and ffmpeg was killed in a 768 MiB
// container; the upload answered 500. Each rendition is now drawn at its own
// size with bounded threads and look-ahead.
func TestRenditionsAreDrawnAtTheirOwnSizeWithBoundedEncoders(t *testing.T) {
	tr := NewTranscoder(&config.VideoConfig{SegmentDurationSec: 4, Bitrates: []string{"500k", "1500k", "4000k"}, FFmpegPath: "ffmpeg"})
	args := tr.buildHLSArgsForVariants("in.mp4", "out", tr.selectVariants(nil), 30)
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "-threads 2 -i in.mp4") {
		t.Fatalf("decoder threads are not bounded: %s", joined[:40])
	}
	for _, want := range []string{"min(iw,480)", "min(ih,480)", "min(iw,720)", "min(ih,720)", "min(iw,1080)", "min(ih,1080)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %s in %s", want, joined)
		}
	}
	if got := strings.Count(joined, "-rc-lookahead 10"); got != 3 {
		t.Errorf("look-ahead bounded in %d of 3 renditions", got)
	}
	if got := strings.Count(joined, "-threads 2"); got != 4 {
		t.Errorf("threads bounded %d times, want 4", got)
	}
}

// A recording from the browser's MediaRecorder - fragmented MP4 with the
// audio track first (h264+opus) and WebM (vp9+opus), paused and resumed -
// transcodes, and a small one is never enlarged. Needs ffmpeg and ffprobe, as
// in the storage image.
func TestTranscodeBrowserRecordings(t *testing.T) {
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip(binary + " is not installed")
		}
	}
	for _, fixture := range []string{"browser-h264-opus.mp4", "browser-vp9-opus.webm"} {
		t.Run(fixture, func(t *testing.T) {
			cfg := &config.VideoConfig{
				SegmentDurationSec: 2, MaxDurationSec: 60, Bitrates: []string{"500k", "1500k", "4000k"},
				FFmpegPath: "ffmpeg", FFprobePath: "ffprobe", ThumbnailTimeSec: 0.5, ThumbnailQScale: 3,
				ThumbnailVariants: []config.ImageVariant{{Key: "180x320", Width: 180, Height: 320}},
			}
			input := filepath.Join(t.TempDir(), fixture)
			data, err := os.ReadFile(filepath.Join("testdata", fixture))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(input, data, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			out := filepath.Join(t.TempDir(), "out")
			result, err := NewTranscoder(cfg).Transcode(ctx, input, out)
			if err != nil {
				t.Fatalf("transcode: %v", err)
			}
			if result.Duration <= 0 || len(result.Variants) == 0 {
				t.Fatalf("result %+v", result)
			}
			for name, dir := range result.Variants {
				if _, err := os.Stat(filepath.Join(dir, "playlist.m3u8")); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				size, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=p=0", filepath.Join(dir, "init.mp4")).Output()
				if err != nil {
					t.Fatalf("%s: probe: %v", name, err)
				}
				parts := strings.Split(strings.TrimSpace(string(size)), ",")
				width, _ := strconv.Atoi(parts[0])
				if width == 0 || width > 320 {
					t.Fatalf("%s rendition is %s wide, source is 320", name, parts[0])
				}
			}
			if _, err := os.Stat(filepath.Join(out, "posters", "180x320.jpg")); err != nil {
				t.Fatalf("poster: %v", err)
			}
		})
	}
}
