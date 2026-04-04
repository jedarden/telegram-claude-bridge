package bridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// videoKeyframeFPS is the frame extraction rate (one frame every N seconds).
	// fps=0.5 means one frame every 2 seconds.
	videoKeyframeFPS = "0.5"

	// videoKeyframeWidth is the width to scale keyframes to. Height is calculated
	// to maintain aspect ratio (-1).
	videoKeyframeWidth = 800

	// maxKeyframes limits the number of keyframes extracted to avoid excessive
	// token usage. A 30-second video at fps=0.5 would produce ~15 frames; we cap
	// at 10 to keep token costs reasonable.
	maxKeyframes = 10
)

// videoResult holds the results of video processing.
type videoResult struct {
	keyframePaths []string // paths to extracted keyframe images
	transcription string   // whisper transcription of the audio track
}

// processVideo downloads a video from the proxy, extracts keyframes and audio,
// and transcribes the audio using Whisper. It returns the video result and a
// list of all temporary files created. The caller must remove these paths
// regardless of whether an error occurred.
func (m *SessionManager) processVideo(
	ctx context.Context,
	chatID, messageID int64,
	fileID string,
) (*videoResult, []string, error) {
	dir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	stem := fmt.Sprintf("%d", messageID)
	videoPath := filepath.Join(dir, stem+".mp4")

	if err := downloadFile(ctx, m.proxyURL+"/file/"+fileID, videoPath); err != nil {
		return nil, nil, fmt.Errorf("download video: %w", err)
	}
	cleanupPaths := []string{videoPath}

	// Extract keyframes.
	keyframePaths, err := extractKeyframes(ctx, videoPath, dir, stem)
	if err != nil {
		return nil, cleanupPaths, fmt.Errorf("extract keyframes: %w", err)
	}
	cleanupPaths = append(cleanupPaths, keyframePaths...)

	// Extract audio track for transcription.
	audioPath := filepath.Join(dir, stem+"_audio.wav")
	if err := extractAudio(ctx, videoPath, audioPath); err != nil {
		return nil, cleanupPaths, fmt.Errorf("extract audio: %w", err)
	}
	cleanupPaths = append(cleanupPaths, audioPath)

	// Transcribe audio using Whisper.
	transcription, err := transcribeAudio(ctx, audioPath, dir, stem)
	if err != nil {
		return nil, cleanupPaths, fmt.Errorf("transcribe audio: %w", err)
	}

	return &videoResult{
		keyframePaths: keyframePaths,
		transcription: transcription,
	}, cleanupPaths, nil
}

// extractKeyframes extracts keyframes from a video file using ffmpeg.
// Returns paths to the extracted frame images.
func extractKeyframes(ctx context.Context, videoPath, dir, stem string) ([]string, error) {
	// Use ffmpeg to extract frames at the specified FPS, scaling to the target width.
	// -vf "fps=0.5,scale=800:-1" means: extract one frame every 2 seconds, scale to 800px width.
	// Frame pattern: {stem}_frame_0001.jpg, {stem}_frame_0002.jpg, etc.
	framePattern := filepath.Join(dir, stem+"_frame_%04d.jpg")

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-i", videoPath,
		"-vf", fmt.Sprintf("fps=%s,scale=%d:-1", videoKeyframeFPS, videoKeyframeWidth),
		"-frames:v", fmt.Sprintf("%d", maxKeyframes),
		"-y", // overwrite existing files
		framePattern,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %w\noutput: %s", err, strings.TrimSpace(string(output)))
	}

	// Find all extracted frames matching the pattern.
	pattern := filepath.Join(dir, stem+"_frame_*.jpg")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob frames: %w", err)
	}
	if len(matches) == 0 {
		// Video may be too short or have no visual content.
		return []string{}, nil
	}

	return matches, nil
}

// extractAudio extracts the audio track from a video file using ffmpeg.
func extractAudio(ctx context.Context, videoPath, audioPath string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-i", videoPath,
		"-vn",               // no video
		"-acodec", "pcm_s16le", // 16-bit little-endian PCM (WAV compatible)
		"-y", // overwrite existing file
		audioPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg: %w\noutput: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// transcribeAudio transcribes an audio file using Whisper CLI.
func transcribeAudio(ctx context.Context, audioPath, dir, stem string) (string, error) {
	// Whisper outputs {stem}.txt in the output_dir when --output_format txt is set.
	txtPath := filepath.Join(dir, stem+"_audio.txt")

	cmd := exec.CommandContext(ctx, "whisper",
		audioPath,
		"--model", "turbo",
		"--output_format", "txt",
		"--output_dir", dir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("whisper: %w\noutput: %s", err, strings.TrimSpace(string(output)))
	}

	data, err := os.ReadFile(txtPath)
	if err != nil {
		return "", fmt.Errorf("read transcription %s: %w", txtPath, err)
	}

	return strings.TrimSpace(string(data)), nil
}
