package video

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/borg001/ipfs-filestorage/internal/config"
)

// VideoInfo — метаданные видео, полученные через ffprobe.
type VideoInfo struct {
	Duration  float64 `json:"duration"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Rotation  int     `json:"rotation"`
	CodecName string  `json:"codec_name"`
	FrameRate float64 `json:"frame_rate"`
	BitRate   int64   `json:"bit_rate"`
	// FormatName is ffprobe's demuxer list, "mov,mp4,m4a,3gp,3g2,mj2" for MP4.
	FormatName string `json:"format_name"`
	// PixFmt, Profile and Level describe the H.264 stream: the master
	// playlist states them, and a browser has to be able to decode them.
	PixFmt  string `json:"pix_fmt"`
	Profile string `json:"profile"`
	Level   int    `json:"level"`
	// AudioCodec is the first audio stream's codec; empty without audio.
	AudioCodec string `json:"audio_codec"`
}

// ValidationError describes a rejected input without exposing an ffprobe or
// filesystem error to an HTTP client. The handler turns Code and limits into a
// localized public response.
type ValidationError struct {
	Code           string
	MaxSizeBytes   int64
	MaxDurationSec int
	MaxLongSide    int
}

func (e *ValidationError) Error() string {
	if e == nil || e.Code == "" {
		return "video validation failed"
	}
	return e.Code
}

// VideoProber — интерфейс для получения метаданных видео.
// В продакшене реализуется *Validator (через ffprobe), в тестах — mock.
type VideoProber interface {
	Probe(ctx context.Context, inputPath string) (*VideoInfo, error)
}

// Validator валидирует видео перед транскодированием.
type Validator struct {
	cfg    *config.VideoConfig
	prober VideoProber
}

// NewValidator создаёт новый Validator.
func NewValidator(cfg *config.VideoConfig) *Validator {
	v := &Validator{cfg: cfg}
	v.prober = v // self — по умолчанию используем реальный ffprobe
	return v
}

// WithProber подставляет мок для тестов.
func (v *Validator) WithProber(p VideoProber) *Validator {
	v.prober = p
	return v
}

// Validate проверяет видео на соответствие требованиям:
//   - размер файла ≤ MaxSizeBytes;
//   - длительность ≤ MaxDurationSec;
//   - MP4 с H.264 (8 бит, 4:2:0) и AAC или без звука: браузер сжимает видео
//     перед загрузкой, хранилище его больше не перекодирует, а только режет
//     на сегменты HLS (theGHub1/docs#201);
//   - длинная сторона ≤ MaxLongSide (720p), не больше 60 кадров в секунду.
//
// The shape of the frame is not one of them. A publication carries the frame
// it is shown in, chosen when it is composed, so a video is accepted as it was
// shot and cropped to that frame where it is presented.
func (v *Validator) Validate(ctx context.Context, inputPath string, fileSize int64) error {
	if fileSize > v.cfg.MaxSizeBytes {
		return &ValidationError{Code: "video_file_too_large", MaxSizeBytes: v.cfg.MaxSizeBytes}
	}

	info, err := v.prober.Probe(ctx, inputPath)
	if err != nil {
		return &ValidationError{Code: "video_metadata_invalid"}
	}

	if info.Duration > float64(v.cfg.MaxDurationSec) {
		return &ValidationError{Code: "video_duration_exceeded", MaxDurationSec: v.cfg.MaxDurationSec}
	}
	if !BrowserPlayable(info) {
		return &ValidationError{Code: "video_encoding_required"}
	}
	if max(info.Width, info.Height) > v.maxLongSide() || info.FrameRate > maxVideoFrameRate {
		return &ValidationError{Code: "video_resolution_too_large", MaxLongSide: v.maxLongSide()}
	}

	return nil
}

const (
	defaultMaxLongSide = 1280
	maxVideoFrameRate  = 60
)

func (v *Validator) maxLongSide() int {
	if v.cfg.MaxLongSide > 0 {
		return v.cfg.MaxLongSide
	}
	return defaultMaxLongSide
}

// BrowserPlayable says the stream can be cut into HLS segments as it is and
// played by every browser: an MP4 with 8-bit 4:2:0 H.264 and AAC or no audio.
// Anything else (HEVC from an iPhone, VP8/VP9 or Opus from a recording, H.264
// High 10) would need a transcode, which the storage no longer runs.
func BrowserPlayable(info *VideoInfo) bool {
	if info == nil || !strings.Contains(","+info.FormatName+",", ",mp4,") {
		return false
	}
	if info.CodecName != "h264" {
		return false
	}
	switch info.PixFmt {
	case "yuv420p", "yuvj420p":
	default:
		return false
	}
	switch info.Profile {
	case "Baseline", "Constrained Baseline", "Main", "High":
	default:
		return false
	}
	return info.AudioCodec == "" || info.AudioCodec == "aac"
}

// Probe получает метаданные видео через ffprobe.
func (v *Validator) Probe(ctx context.Context, inputPath string) (*VideoInfo, error) {
	return probeVideoInfo(ctx, v.cfg.FFprobePath, inputPath)
}

// probeVideoInfo reads the video stream metadata used by both validation and
// transcoding. Keeping one parser prevents the output ladder from disagreeing
// with the validation that accepted the original upload.
func probeVideoInfo(ctx context.Context, ffprobePath, inputPath string) (*VideoInfo, error) {
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error",
		"-show_streams",
		"-show_format",
		"-of", "json",
		inputPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var result struct {
		Format struct {
			Duration   string `json:"duration"`
			BitRate    string `json:"bit_rate"`
			FormatName string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			CodecType   string `json:"codec_type"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
			PixFmt    string `json:"pix_fmt"`
			Profile   string `json:"profile"`
			Level     int    `json:"level"`
			Duration  string `json:"duration"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			CodecName string `json:"codec_name"`
			FrameRate string `json:"avg_frame_rate"`
			Tags      struct {
				Rotate string `json:"rotate"`
			} `json:"tags"`
			SideDataList []struct {
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}
	videoStream, audioCodec := -1, ""
	for i, candidate := range result.Streams {
		switch candidate.CodecType {
		case "video":
			// Cover art is stored as a one-frame video stream; it is not the video.
			if videoStream < 0 && candidate.Disposition.AttachedPic == 0 {
				videoStream = i
			}
		case "audio":
			if audioCodec == "" {
				audioCodec = candidate.CodecName
			}
		}
	}
	if videoStream < 0 {
		return nil, fmt.Errorf("no video streams found")
	}

	stream := result.Streams[videoStream]
	duration := result.Format.Duration
	if duration == "" {
		duration = stream.Duration
	}
	parsedDuration, err := strconv.ParseFloat(strings.TrimSpace(duration), 64)
	if err != nil || parsedDuration <= 0 {
		// A video recorded in a browser (MediaRecorder WebM) carries no
		// duration in its header: it is read from the last frame instead.
		parsedDuration, err = probePacketDuration(ctx, ffprobePath, inputPath)
		if err != nil {
			return nil, fmt.Errorf("parse duration: %w", err)
		}
	}
	rotation, _ := strconv.Atoi(strings.TrimSpace(stream.Tags.Rotate))
	for _, sideData := range stream.SideDataList {
		if sideData.Rotation != 0 {
			rotation = int(sideData.Rotation)
			break
		}
	}
	frameRate := parseFrameRate(stream.FrameRate)
	bitRate, _ := strconv.ParseInt(strings.TrimSpace(result.Format.BitRate), 10, 64)
	return &VideoInfo{
		Duration:  parsedDuration,
		Width:     stream.Width,
		Height:    stream.Height,
		Rotation:  rotation,
		CodecName: stream.CodecName,
		FrameRate: frameRate,
		BitRate:   bitRate,

		FormatName: result.Format.FormatName,
		PixFmt:     stream.PixFmt,
		Profile:    stream.Profile,
		Level:      stream.Level,
		AudioCodec: audioCodec,
	}, nil
}

func parseFrameRate(value string) float64 {
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) != 2 {
		return 0
	}
	numerator, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	denominator, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || denominator <= 0 {
		return 0
	}
	return numerator / denominator
}

// probePacketDuration measures a video that does not state its duration: the
// end of its last video frame, read from the packets.
func probePacketDuration(ctx context.Context, ffprobePath, inputPath string) (float64, error) {
	out, err := exec.CommandContext(ctx, ffprobePath,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time,duration_time",
		"-of", "csv=p=0",
		inputPath,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe packets: %w", err)
	}
	return lastFrameEnd(string(out))
}

// lastFrameEnd is the latest frame start plus its length, from ffprobe's
// "pts_time,duration_time" lines; a frame without either is skipped.
func lastFrameEnd(lines string) (float64, error) {
	end := 0.0
	for _, line := range strings.Split(lines, "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		pts, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		length := 0.0
		if len(fields) > 1 {
			length, _ = strconv.ParseFloat(fields[1], 64)
		}
		if pts+length > end {
			end = pts + length
		}
	}
	if end <= 0 {
		return 0, fmt.Errorf("no timed video frames")
	}
	return end, nil
}
