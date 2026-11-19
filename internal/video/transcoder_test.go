package video

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/borg001/ipfs-filestorage/internal/config"
)

// The storage cuts a video into segments without encoding it: ffmpeg copies
// the streams. Three x264 renditions on two CPUs starved serving and the
// health check, and every image failed to load (theGHub1/docs#201).
func TestRemuxCopiesStreamsWithoutEncoding(t *testing.T) {
	tr := NewTranscoder(&config.VideoConfig{SegmentDurationSec: 4, FFmpegPath: "ffmpeg"})
	args := tr.buildRemuxArgs("in.mp4", "out/medium")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-c copy", "-map 0:v:0", "-map 0:a:0?", "-map_metadata -1", "-hls_segment_type fmp4", "-hls_time 4", "-threads 1", "out/medium/seg_%d.m4s", "out/medium/playlist.m3u8"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in %s", want, joined)
		}
	}
	for _, encoder := range []string{"libx264", "-c:v", "-b:v", "-vf", "scale"} {
		if strings.Contains(joined, encoder) {
			t.Errorf("remux encodes (%s): %s", encoder, joined)
		}
	}
}

// One frame is decoded and scaled to every poster size in one ffmpeg run.
func TestBuildThumbnailArgsDecodesOneFrame(t *testing.T) {
	tr := NewTranscoder(&config.VideoConfig{
		FFmpegPath: "ffmpeg", ThumbnailTimeSec: 1.25, ThumbnailQScale: 4,
		ThumbnailVariants: []config.ImageVariant{{Key: "180x320", Width: 180, Height: 320}, {Key: "720x1280", Width: 720, Height: 1280}},
	})
	args := tr.buildThumbnailArgs("in.mp4", "posters", 10)
	joined := strings.Join(args, " ")
	if strings.Count(joined, "-i ") != 1 || !strings.Contains(joined, "-ss 1.250") {
		t.Fatalf("one input seeked to the poster time expected: %s", joined)
	}
	for _, want := range []string{"split=2", "scale=180:320:force_original_aspect_ratio=decrease", "scale=720:1280:force_original_aspect_ratio=decrease", "-q:v 4", "posters/180x320.jpg", "posters/720x1280.jpg"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in %s", want, joined)
		}
	}
	if got := strings.Count(joined, "-frames:v 1"); got != 2 {
		t.Errorf("single frame per poster: %d of 2", got)
	}
	// A clip shorter than the poster time is seeked to its start.
	if short := strings.Join(tr.buildThumbnailArgs("in.mp4", "posters", 0.8), " "); !strings.Contains(short, "-ss 0.000") {
		t.Errorf("short clip: %s", short)
	}
}

func TestStreamInfoStatesTheRealStream(t *testing.T) {
	got := streamInfo(&VideoInfo{Width: 1280, Height: 720, Rotation: 90, BitRate: 2000000, Profile: "High", Level: 31, AudioCodec: "aac"})
	want := StreamInfo{Bandwidth: 2400000, Resolution: "720x1280", Codecs: "avc1.64001f,mp4a.40.2"}
	if got != want {
		t.Fatalf("streamInfo = %+v, want %+v", got, want)
	}
	if got := streamInfo(&VideoInfo{Width: 640, Height: 360, Profile: "Constrained Baseline", Level: 30}); got.Codecs != "avc1.42e01e" || got.Bandwidth != 3000000 {
		t.Fatalf("no sound, unknown bit rate: %+v", got)
	}
}

func TestThumbnailFilename(t *testing.T) {
	got := thumbnailFilename(config.ImageVariant{Key: "180x320", Width: 180, Height: 320})
	if got != "180x320.jpg" {
		t.Errorf("thumbnailFilename = %q, want 180x320.jpg", got)
	}
	got = thumbnailFilename(config.ImageVariant{Width: 360, Height: 640})
	if got != "360x640.jpg" {
		t.Errorf("thumbnailFilename fallback = %q, want 360x640.jpg", got)
	}
}

func TestTranscodeMissingInput(t *testing.T) {
	cfg := &config.VideoConfig{SegmentDurationSec: 4, FFmpegPath: "ffmpeg", FFprobePath: "ffprobe", MaxDurationSec: 60}
	if _, err := NewTranscoder(cfg).Transcode(context.Background(), "/nonexistent/input.mp4", t.TempDir()); err == nil {
		t.Error("Expected error for missing input file")
	}
}

func TestProbeDurationInvalidPath(t *testing.T) {
	tr := NewTranscoder(&config.VideoConfig{FFprobePath: "ffprobe"})
	if _, err := tr.probeDuration(context.Background(), "/nonexistent/file.mp4"); err == nil {
		t.Error("Expected error for nonexistent file in probeDuration")
	}
}

func needFFmpeg(t *testing.T) {
	t.Helper()
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip(binary + " is not installed")
		}
	}
}

func remuxConfig() *config.VideoConfig {
	return &config.VideoConfig{
		SegmentDurationSec: 2, MaxDurationSec: 60, MaxSizeBytes: 100 << 20, MaxLongSide: 1280,
		FFmpegPath: "ffmpeg", FFprobePath: "ffprobe", ThumbnailTimeSec: 0.5, ThumbnailQScale: 3,
		ThumbnailVariants: []config.ImageVariant{{Key: "180x320", Width: 180, Height: 320}, {Key: "360x640", Width: 360, Height: 640}},
	}
}

// A video as the browser compresses it - 720p H.264 + AAC MP4 - is cut into
// segments that carry the same stream, with the posters, in a fraction of
// its length. Needs ffmpeg and ffprobe, as in the storage image.
func TestTranscodeRemuxesACompressedVideo(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "phone.mp4")
	make := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=720x1280:rate=30", "-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "6", "-c:v", "libx264", "-preset", "veryfast", "-g", "60", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-metadata", "location=+55.7558+037.6173/", input)
	if out, err := make.CombinedOutput(); err != nil {
		t.Fatalf("make fixture: %v %s", err, out)
	}
	cfg := remuxConfig()
	if err := NewValidator(cfg).Validate(context.Background(), input, 1024); err != nil {
		t.Fatalf("validate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out := filepath.Join(dir, "out")
	result, err := NewTranscoder(cfg).Transcode(ctx, input, out)
	if err != nil {
		t.Fatalf("transcode: %v", err)
	}
	if len(result.Variants) != 1 || result.Variants["medium"] == "" || result.Duration < 5.9 {
		t.Fatalf("result %+v", result)
	}
	stream := result.Streams["medium"]
	if stream.Resolution != "720x1280" || !strings.HasPrefix(stream.Codecs, "avc1.64") || !strings.HasSuffix(stream.Codecs, ",mp4a.40.2") {
		t.Fatalf("stream info %+v", stream)
	}
	variant := result.Variants["medium"]
	playlist, err := os.ReadFile(filepath.Join(variant, "playlist.m3u8"))
	if err != nil || !strings.Contains(string(playlist), "seg_0.m4s") || !strings.Contains(string(playlist), "#EXT-X-MAP:URI=\"init.mp4\"") {
		t.Fatalf("playlist: %v\n%s", err, playlist)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,width,height:format_tags", "-of", "compact=p=0", filepath.Join(variant, "init.mp4")).Output()
	if err != nil {
		t.Fatalf("probe segment: %v", err)
	}
	if text := string(probe); !strings.Contains(text, "codec_name=h264|width=720|height=1280") || !strings.Contains(text, "codec_name=aac") || strings.Contains(text, "location") {
		t.Fatalf("segment is not the copied stream, or keeps the location: %s", text)
	}
	for _, poster := range []string{"180x320.jpg", "360x640.jpg"} {
		if _, err := os.Stat(filepath.Join(out, "posters", poster)); err != nil {
			t.Fatalf("poster: %v", err)
		}
	}
}

// A browser recording that was not compressed - H.264 with Opus, VP9 WebM -
// is refused by validation and never reaches ffmpeg as an encode.
func TestBrowserRecordingsNeedCompression(t *testing.T) {
	needFFmpeg(t)
	for _, fixture := range []string{"browser-h264-opus.mp4", "browser-vp9-opus.webm"} {
		t.Run(fixture, func(t *testing.T) {
			input := filepath.Join("testdata", fixture)
			cfg := remuxConfig()
			err := NewValidator(cfg).Validate(context.Background(), input, 1024)
			if validationError, ok := err.(*ValidationError); !ok || validationError.Code != "video_encoding_required" {
				t.Fatalf("validate = %v, want video_encoding_required", err)
			}
			if _, err := NewTranscoder(cfg).Transcode(context.Background(), input, filepath.Join(t.TempDir(), "out")); err == nil {
				t.Fatal("transcode accepted a stream it would have to encode")
			}
		})
	}
}
