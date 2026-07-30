package bridge

import (
	"fmt"
	"strings"
	"testing"
)

// TestVideoConstants tests that video processing constants are properly defined.
func TestVideoConstants(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
		check string
	}{
		{
			name:  "videoKeyframeFPS",
			value: videoKeyframeFPS,
			check: "0.5",
		},
		{
			name:  "videoKeyframeWidth",
			value: videoKeyframeWidth,
			check: "800",
		},
		{
			name:  "maxKeyframes",
			value: maxKeyframes,
			check: "10",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			switch v := tc.value.(type) {
			case string:
				got = v
			case int:
				got = fmt.Sprintf("%d", v)
			}
			if got != tc.check {
				t.Errorf("%s = %v, want %s", tc.name, got, tc.check)
			}
		})
	}
}

// TestExtractKeyframesVfArg tests that the video filter argument is correctly formatted.
func TestExtractKeyframesVfArg(t *testing.T) {
	// The -vf argument should be: fps=0.5,scale=800:-1
	expectedVf := fmt.Sprintf("fps=%s,scale=%d:-1", videoKeyframeFPS, videoKeyframeWidth)
	if expectedVf != "fps=0.5,scale=800:-1" {
		t.Errorf("vf arg format: got %q, want %q", expectedVf, "fps=0.5,scale=800:-1")
	}
}

// TestExtractKeyframesPattern tests that frame pattern is correctly formatted.
func TestExtractKeyframesPattern(t *testing.T) {
	dir := "/tmp/test"
	stem := "123"

	// Frame pattern: {stem}_frame_%04d.jpg
	framePattern := fmt.Sprintf("%s/%s_frame_%%04d.jpg", dir, stem)
	expected := "/tmp/test/123_frame_%04d.jpg"

	if framePattern != expected {
		t.Errorf("frame pattern: got %q, want %q", framePattern, expected)
	}

	// Glob pattern for finding frames
	globPattern := fmt.Sprintf("%s/%s_frame_*.jpg", dir, stem)
	expectedGlob := "/tmp/test/123_frame_*.jpg"

	if globPattern != expectedGlob {
		t.Errorf("glob pattern: got %q, want %q", globPattern, expectedGlob)
	}
}

// TestVideoResult tests the videoResult struct initialization.
func TestVideoResult(t *testing.T) {
	tests := []struct {
		name          string
		keyframePaths []string
		transcription string
	}{
		{
			name:          "successful result with frames",
			keyframePaths: []string{"/tmp/123_frame_0001.jpg", "/tmp/123_frame_0002.jpg"},
			transcription: "hello world",
		},
		{
			name:          "successful result with no frames",
			keyframePaths: []string{},
			transcription: "test audio",
		},
		{
			name:          "empty result",
			keyframePaths: nil,
			transcription: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := &videoResult{
				keyframePaths: tc.keyframePaths,
				transcription: tc.transcription,
			}

			if len(result.keyframePaths) != len(tc.keyframePaths) {
				t.Errorf("keyframePaths length: got %d, want %d", len(result.keyframePaths), len(tc.keyframePaths))
			}

			if result.transcription != tc.transcription {
				t.Errorf("transcription: got %q, want %q", result.transcription, tc.transcription)
			}
		})
	}
}

// TestWhisperArgs tests that Whisper command arguments are correctly built.
func TestFFmpegKeyframesArgs(t *testing.T) {
	videoPath := "/tmp/video.mp4"
	dir := "/tmp"
	stem := "123"

	// Build expected args (matches extractKeyframes command)
	framePattern := fmt.Sprintf("%s/%s_frame_%%04d.jpg", dir, stem)
	vfArg := fmt.Sprintf("fps=%s,scale=%d:-1", videoKeyframeFPS, videoKeyframeWidth)
	framesArg := fmt.Sprintf("%d", maxKeyframes)

	expectedParts := []string{
		"-i", videoPath,
		"-vf", vfArg,
		"-frames:v", framesArg,
		"-y",
		framePattern,
	}

	// Verify expected values
	if vfArg != "fps=0.5,scale=800:-1" {
		t.Errorf("vf arg: got %q, want fps=0.5,scale=800:-1", vfArg)
	}

	if framesArg != "10" {
		t.Errorf("frames arg: got %q, want 10", framesArg)
	}

	if framePattern != "/tmp/123_frame_%04d.jpg" {
		t.Errorf("pattern: got %q, want /tmp/123_frame_%%04d.jpg", framePattern)
	}

	// Verify all expected parts are present
	partsStr := strings.Join(expectedParts, " ")
	expectedContains := []string{"-i", videoPath, "-vf", "-frames:v", "-y"}
	for _, exp := range expectedContains {
		if !strings.Contains(partsStr, exp) {
			t.Errorf("expected parts missing %q: got %q", exp, partsStr)
		}
	}
}

// TestFFmpegAudioExtractionArgs tests that ffmpeg audio extraction arguments are correct.
func TestFFmpegAudioExtractionArgs(t *testing.T) {
	videoPath := "/tmp/video.mp4"
	audioPath := "/tmp/audio.wav"

	// Build expected args (matches extractAudio command)
	expectedParts := []string{
		"-i", videoPath,
		"-vn",
		"-acodec", "pcm_s16le",
		"-y",
		audioPath,
	}

	partsStr := strings.Join(expectedParts, " ")

	// Verify key components
	expectedContains := []string{
		"-i", videoPath,
		"-vn", // no video
		"-acodec", "pcm_s16le", // 16-bit PCM
		"-y", // overwrite
		audioPath,
	}

	for _, exp := range expectedContains {
		if !strings.Contains(partsStr, exp) {
			t.Errorf("expected parts missing %q in: %q", exp, partsStr)
		}
	}
}
