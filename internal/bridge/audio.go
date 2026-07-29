package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// processAudio downloads a voice/audio file from the proxy and transcribes it
// using the Whisper CLI. It returns the transcription text and a list of all
// temporary files created (audio file + Whisper output). The caller must remove
// these paths regardless of whether an error occurred.
func (m *SessionManager) processAudio(
	ctx context.Context,
	chatID, messageID int64,
	content *contract.Content,
) (transcription string, cleanupPaths []string, err error) {
	dir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	stem := fmt.Sprintf("%d", messageID)
	mimeType := ""
	if content.MimeType != nil {
		mimeType = *content.MimeType
	}
	ext := audioFileExt(content.Type, mimeType)
	audioPath := filepath.Join(dir, stem+"."+ext)

	if err := downloadFile(ctx, m.proxyURL+"/file/"+*content.FileID, audioPath); err != nil {
		return "", nil, fmt.Errorf("download audio: %w", err)
	}
	cleanupPaths = append(cleanupPaths, audioPath)

	// Whisper outputs {stem}.txt in the output_dir when --output_format txt is set.
	txtPath := filepath.Join(dir, stem+".txt")
	cleanupPaths = append(cleanupPaths, txtPath)

	cmd := m.commandExec.CommandContext(ctx, "whisper",
		audioPath,
		"--model", "turbo",
		"--output_format", "txt",
		"--output_dir", dir,
	)
	out, cmdErr := cmd.CombinedOutput()
	if cmdErr != nil {
		return "", cleanupPaths, fmt.Errorf("whisper: %w\noutput: %s", cmdErr, strings.TrimSpace(string(out)))
	}

	data, err := os.ReadFile(txtPath)
	if err != nil {
		return "", cleanupPaths, fmt.Errorf("read transcription %s: %w", txtPath, err)
	}

	return strings.TrimSpace(string(data)), cleanupPaths, nil
}

// audioFileExt returns a file extension suitable for saving audio of the given
// content type. mimeType is used as a hint when the content type is "audio".
func audioFileExt(contentType, mimeType string) string {
	if contentType == contract.ContentTypeVoice {
		return "ogg" // Telegram voice messages are always OGG/Opus
	}
	switch mimeType {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return "m4a"
	case "audio/flac":
		return "flac"
	case "audio/ogg":
		return "ogg"
	case "audio/wav", "audio/x-wav":
		return "wav"
	}
	return "mp3" // sensible default for audio files
}

// startTyping sends a continuous typing indicator until the returned stop
// function is called. The first indicator fires immediately; subsequent ones
// fire every 4 seconds (Telegram's typing action expires after ~5 seconds).
// The goroutine also exits when ctx is cancelled.
func (m *SessionManager) startTyping(ctx context.Context, chatID int64, tidPtr *int64) func() {
	stop := make(chan struct{})
	m.sender.SendTyping(ctx, chatID, tidPtr)
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.sender.SendTyping(ctx, chatID, tidPtr)
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() {
		select {
		case <-stop: // already stopped
		default:
			close(stop)
		}
	}
}
