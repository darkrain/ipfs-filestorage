package video

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/borg001/ipfs-filestorage/internal/config"
)

// TranscodeResult — результат нарезки видео в HLS/CMAF.
type TranscodeResult struct {
	// OutputDir — директория со сгенерированными файлами.
	OutputDir string
	// Variants — мапа: имя варианта → поддиректория. У нового видео он один.
	Variants map[string]string
	// Streams describes each variant for the master playlist.
	Streams map[string]StreamInfo
	// Duration — длительность видео в секундах.
	Duration float64
}

// StreamInfo is what the master playlist states about a variant.
type StreamInfo struct {
	Bandwidth  int64
	Resolution string
	Codecs     string
}

// remuxVariant names the one variant of a video compressed in the browser
// (720p): it sits where the "medium" rendition of the old ladder was.
const remuxVariant = "medium"

// Transcoder cuts an uploaded video into HLS segments. It no longer encodes:
// the browser compresses every video to 720p H.264 + AAC before upload, and
// ffmpeg only copies the streams into fMP4 segments and takes one frame for
// the posters (theGHub1/docs#201). Encoding three renditions on the storage's
// two CPUs starved serving: the health check timed out and every image on
// the platform failed to load, and ffmpeg was killed for memory (#194).
type Transcoder struct {
	cfg *config.VideoConfig
}

// NewTranscoder создаёт новый Transcoder.
func NewTranscoder(cfg *config.VideoConfig) *Transcoder {
	return &Transcoder{cfg: cfg}
}

// Transcode cuts the video into one HLS (fMP4 CMAF) variant without
// re-encoding and draws the posters.
// inputPath — путь к исходному видеофайлу.
// outputDir — директория для результата (будет создана).
func (t *Transcoder) Transcode(ctx context.Context, inputPath, outputDir string) (*TranscodeResult, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	info, err := probeVideoInfo(ctx, t.cfg.FFprobePath, inputPath)
	if err != nil {
		return nil, fmt.Errorf("probe duration: %w", err)
	}
	if info.Duration > float64(t.cfg.MaxDurationSec) {
		return nil, fmt.Errorf("video duration %.1fs exceeds max %ds", info.Duration, t.cfg.MaxDurationSec)
	}
	// Validation has accepted the file already; a stream that cannot be
	// copied as it is never reaches ffmpeg as an encode.
	if !BrowserPlayable(info) {
		return nil, fmt.Errorf("video is not browser-playable H.264/AAC MP4")
	}

	if err := t.generateThumbnails(ctx, inputPath, outputDir, info.Duration); err != nil {
		return nil, fmt.Errorf("generate thumbnails: %w", err)
	}

	variantDir := filepath.Join(outputDir, remuxVariant)
	if err := os.MkdirAll(variantDir, 0o755); err != nil {
		return nil, fmt.Errorf("create variant dir %s: %w", variantDir, err)
	}
	cmd := exec.CommandContext(ctx, t.cfg.FFmpegPath, t.buildRemuxArgs(inputPath, variantDir)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg failed: %w\noutput: %s", err, tail(output, 4000))
	}

	return &TranscodeResult{
		OutputDir: outputDir,
		Variants:  map[string]string{remuxVariant: variantDir},
		Streams:   map[string]StreamInfo{remuxVariant: streamInfo(info)},
		Duration:  info.Duration,
	}, nil
}

// buildRemuxArgs copies the first video and audio streams into fMP4 HLS
// segments. Nothing is decoded or encoded; the metadata of the file (a
// phone's location, its model) is dropped.
func (t *Transcoder) buildRemuxArgs(inputPath, variantDir string) []string {
	segDur := t.cfg.SegmentDurationSec
	if segDur <= 0 {
		segDur = 4
	}
	return []string{
		"-nostdin",
		"-threads", "1",
		"-i", inputPath,
		"-y",
		"-map", "0:v:0",
		"-map", "0:a:0?",
		"-c", "copy",
		"-map_metadata", "-1",
		"-map_chapters", "-1",
		"-f", "hls",
		"-hls_time", strconv.Itoa(segDur),
		"-hls_list_size", "0",
		"-hls_playlist_type", "vod",
		"-hls_flags", "independent_segments",
		"-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(variantDir, "seg_%d.m4s"),
		filepath.Join(variantDir, "playlist.m3u8"),
	}
}

// streamInfo states the variant as it is: its real size, bit rate and codecs,
// so a player picks it and the browser knows it can decode it.
func streamInfo(info *VideoInfo) StreamInfo {
	width, height := info.Width, info.Height
	if info.Rotation == 90 || info.Rotation == -90 || info.Rotation == 270 || info.Rotation == -270 {
		width, height = height, width
	}
	bandwidth := info.BitRate
	if bandwidth <= 0 {
		bandwidth = 2500000
	}
	codecs := avcCodec(info.Profile, info.Level)
	if info.AudioCodec == "aac" {
		codecs += ",mp4a.40.2"
	}
	return StreamInfo{
		// A segment peaks above the average rate; 20% keeps a player from
		// choosing a variant its connection cannot hold.
		Bandwidth:  bandwidth + bandwidth/5,
		Resolution: fmt.Sprintf("%dx%d", width, height),
		Codecs:     codecs,
	}
}

// avcCodec is the RFC 6381 name of an H.264 stream: profile_idc,
// constraint flags and level_idc in hex.
func avcCodec(profile string, level int) string {
	if level <= 0 {
		level = 31
	}
	prefix := "6400" // High
	switch profile {
	case "Baseline":
		prefix = "4200"
	case "Constrained Baseline":
		prefix = "42e0"
	case "Main":
		prefix = "4d40"
	}
	return fmt.Sprintf("avc1.%s%02x", prefix, level)
}

func tail(output []byte, limit int) string {
	if len(output) > limit {
		output = output[len(output)-limit:]
	}
	return string(output)
}

// generateThumbnails decodes one frame and scales it to every poster size in
// one ffmpeg run.
func (t *Transcoder) generateThumbnails(ctx context.Context, inputPath, outputDir string, duration float64) error {
	if len(t.cfg.ThumbnailVariants) == 0 {
		return nil
	}
	postersDir := filepath.Join(outputDir, "posters")
	if err := os.MkdirAll(postersDir, 0o755); err != nil {
		return fmt.Errorf("create posters dir: %w", err)
	}
	args := t.buildThumbnailArgs(inputPath, postersDir, duration)
	if args == nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, t.cfg.FFmpegPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg posters failed: %w\noutput: %s", err, tail(output, 4000))
	}
	return nil
}

func (t *Transcoder) buildThumbnailArgs(inputPath, postersDir string, duration float64) []string {
	var variants []config.ImageVariant
	for _, variant := range t.cfg.ThumbnailVariants {
		if variant.Width > 0 && variant.Height > 0 {
			variants = append(variants, variant)
		}
	}
	if len(variants) == 0 {
		return nil
	}
	// A clip shorter than the poster time has no frame there.
	seek := t.cfg.ThumbnailTimeSec
	if duration > 0 && seek >= duration {
		seek = 0
	}
	// The variant is a bounding box, not a shape: a poster cropped to one shape
	// would show a different picture than the video it stands for.
	var filter strings.Builder
	filter.WriteString(fmt.Sprintf("[0:v:0]split=%d", len(variants)))
	for i := range variants {
		filter.WriteString(fmt.Sprintf("[s%d]", i))
	}
	for i, variant := range variants {
		filter.WriteString(fmt.Sprintf(";[s%d]scale=%d:%d:force_original_aspect_ratio=decrease[p%d]", i, variant.Width, variant.Height, i))
	}
	args := []string{
		"-nostdin",
		"-threads", "1",
		"-ss", fmt.Sprintf("%.3f", seek),
		"-i", inputPath,
		"-filter_complex", filter.String(),
		"-y",
	}
	qscale := strconv.Itoa(t.cfg.ThumbnailQScale)
	for i, variant := range variants {
		args = append(args, "-map", fmt.Sprintf("[p%d]", i), "-frames:v", "1", "-q:v", qscale,
			filepath.Join(postersDir, thumbnailFilename(variant)))
	}
	return args
}

func thumbnailFilename(variant config.ImageVariant) string {
	key := strings.TrimSpace(variant.Key)
	if key == "" {
		key = fmt.Sprintf("%dx%d", variant.Width, variant.Height)
	}
	key = strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_").Replace(key)
	return key + ".jpg"
}

func (t *Transcoder) probeDuration(ctx context.Context, inputPath string) (float64, error) {
	info, err := probeVideoInfo(ctx, t.cfg.FFprobePath, inputPath)
	if err != nil {
		return 0, err
	}
	return info.Duration, nil
}
