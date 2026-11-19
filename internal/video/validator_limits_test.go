package video

import (
	"context"
	"errors"
	"testing"

	"github.com/borg001/ipfs-filestorage/internal/config"
)

// The storage no longer encodes (theGHub1/docs#201): only a video the
// browser has compressed - MP4, 8-bit 4:2:0 H.264, AAC or no sound - is cut
// into segments as it is. Everything else is refused before ffmpeg runs.
func TestValidateRefusesVideoThatNeedsEncoding(t *testing.T) {
	cfg := &config.VideoConfig{MaxSizeBytes: 100 * 1024 * 1024, MaxDurationSec: 60}
	cases := map[string]func(*VideoInfo){
		"HEVC from an iPhone":   func(i *VideoInfo) { i.CodecName = "hevc"; i.Profile = "Main" },
		"VP9 WebM recording":    func(i *VideoInfo) { i.FormatName = "matroska,webm"; i.CodecName = "vp9" },
		"Opus sound":            func(i *VideoInfo) { i.AudioCodec = "opus" },
		"H.264 High 10":         func(i *VideoInfo) { i.Profile = "High 10"; i.PixFmt = "yuv420p10le" },
		"H.264 4:4:4":           func(i *VideoInfo) { i.Profile = "High 4:4:4 Predictive"; i.PixFmt = "yuv444p" },
		"AVI with H.264":        func(i *VideoInfo) { i.FormatName = "avi" },
		"Matroska with H.264":   func(i *VideoInfo) { i.FormatName = "matroska,webm" },
		"MP3 sound":             func(i *VideoInfo) { i.AudioCodec = "mp3" },
		"unknown codec profile": func(i *VideoInfo) { i.Profile = "" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			info := playable(VideoInfo{Duration: 10, Width: 720, Height: 1280, FrameRate: 30})
			change(info)
			v := &Validator{cfg: cfg, prober: &mockProber{info: info}}
			var validationError *ValidationError
			if err := v.Validate(context.Background(), "/some/file.mp4", 1024); !errors.As(err, &validationError) || validationError.Code != "video_encoding_required" {
				t.Fatalf("err = %v, want video_encoding_required", err)
			}
		})
	}
}

// A compressed video is accepted with sound or without, in every profile a
// browser decodes.
func TestValidateAcceptsBrowserCompressedVideo(t *testing.T) {
	cfg := &config.VideoConfig{MaxSizeBytes: 100 * 1024 * 1024, MaxDurationSec: 60}
	for _, change := range []func(*VideoInfo){
		func(i *VideoInfo) {},
		func(i *VideoInfo) { i.AudioCodec = "" },
		func(i *VideoInfo) { i.Profile = "Constrained Baseline" },
		func(i *VideoInfo) { i.Profile = "Main"; i.PixFmt = "yuvj420p" },
	} {
		info := playable(VideoInfo{Duration: 10, Width: 1280, Height: 720, FrameRate: 30})
		change(info)
		v := &Validator{cfg: cfg, prober: &mockProber{info: info}}
		if err := v.Validate(context.Background(), "/some/file.mp4", 1024); err != nil {
			t.Fatalf("%+v: %v", info, err)
		}
	}
}

// A frame larger than 720p or faster than 60 fps is refused: the browser
// should have compressed it.
func TestValidateRefusesFramesLargerThan720p(t *testing.T) {
	cfg := &config.VideoConfig{MaxSizeBytes: 100 * 1024 * 1024, MaxDurationSec: 60, MaxLongSide: 1280}
	for _, info := range []*VideoInfo{
		playable(VideoInfo{Duration: 10, Width: 1920, Height: 1080}),
		playable(VideoInfo{Duration: 10, Width: 1080, Height: 1920}),
		playable(VideoInfo{Duration: 10, Width: 1281, Height: 720}),
		playable(VideoInfo{Duration: 10, Width: 1280, Height: 720, FrameRate: 120}),
	} {
		v := &Validator{cfg: cfg, prober: &mockProber{info: info}}
		var validationError *ValidationError
		if err := v.Validate(context.Background(), "/some/file.mp4", 1024); !errors.As(err, &validationError) || validationError.Code != "video_resolution_too_large" || validationError.MaxLongSide != 1280 {
			t.Fatalf("%+v: err = %v, want video_resolution_too_large", info, err)
		}
	}
}
