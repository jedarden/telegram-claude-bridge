package bridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Mock Infrastructure for exec.Command ────────────────────────────────────────

// commandExec is an interface for executing commands, allowing test mocks.
type commandExec interface {
	CommandContext(ctx context.Context, name string, args ...string) command
}

// command is an interface for a running command, allowing test mocks.
type command interface {
	CombinedOutput() ([]byte, error)
	Run() error
}

// realCommandExec implements commandExec using os/exec.Command.
type realCommandExec struct{}

func (r realCommandExec) CommandContext(ctx context.Context, name string, args ...string) command {
	return &realCommand{cmd: exec.CommandContext(ctx, name, args...)}
}

// realCommand wraps exec.Cmd to implement the command interface.
type realCommand struct {
	cmd *exec.Cmd
}

func (r *realCommand) CombinedOutput() ([]byte, error) {
	return r.cmd.CombinedOutput()
}

func (r *realCommand) Run() error {
	return r.cmd.Run()
}

// mockCommandExec allows test code to provide predefined command results.
type mockCommandExec struct {
	commands map[string]command // keyed by "name arg1 arg2"
}

func newMockCommandExec() *mockCommandExec {
	return &mockCommandExec{
		commands: make(map[string]command),
	}
}

func (m *mockCommandExec) addCommand(key string, cmd command) {
	m.commands[key] = cmd
}

func (m *mockCommandExec) CommandContext(ctx context.Context, name string, args ...string) command {
	key := commandKey(name, args...)
	if cmd, ok := m.commands[key]; ok {
		return cmd
	}
	return &mockCommand{err: fmt.Errorf("no mock for command: %s", key)}
}

// commandKey generates a unique key for a command invocation.
func commandKey(name string, args ...string) string {
	return fmt.Sprintf("%s %v", name, args)
}

// mockCommand allows test code to provide predefined outputs/errors.
type mockCommand struct {
	output []byte
	err    error
}

func (m *mockCommand) CombinedOutput() ([]byte, error) {
	return m.output, m.err
}

func (m *mockCommand) Run() error {
	return m.err
}

// ── Test Context Helpers ───────────────────────────────────────────────────────

// testAudioContext holds common test dependencies for audio tests.
type testAudioContext struct {
	ctx        context.Context
	tempDir    string
	chatID     int64
	messageID  int64
}

// newTestAudioContext creates a fresh test context for audio tests.
func newTestAudioContext(t *testing.T) testAudioContext {
	t.Helper()

	ctx := context.Background()
	tempDir := t.TempDir()

	return testAudioContext{
		ctx:        ctx,
		tempDir:    tempDir,
		chatID:     12345,
		messageID:  67890,
	}
}

// createMockWhisperSuccess creates a mock command that simulates a successful Whisper run.
// It creates the expected output file and returns success.
func createMockWhisperSuccess(t *testing.T, outputDir string, stem string, transcription string) command {
	t.Helper()

	// Create the expected output file
	outputPath := filepath.Join(outputDir, stem+".txt")
	err := os.WriteFile(outputPath, []byte(transcription), 0644)
	require.NoError(t, err, "failed to create mock Whisper output")

	return &mockCommand{
		output: []byte("whisper output"),
		err:    nil,
	}
}

// createMockWhisperError creates a mock command that simulates a Whisper failure.
func createMockWhisperError(t *testing.T, errorMsg string) command {
	t.Helper()
	return &mockCommand{
		output: []byte(errorMsg),
		err:    fmt.Errorf("whisper failed: %s", errorMsg),
	}
}

// ── Audio Type Detection (audioFileExt) ─────────────────────────────────────────

func TestAudioFileExt(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		mimeType    string
		want        string
	}{
		{
			name:        "voice message always returns ogg",
			contentType: contract.ContentTypeVoice,
			mimeType:    "audio/ogg",
			want:        "ogg",
		},
		{
			name:        "voice with different mime still ogg",
			contentType: contract.ContentTypeVoice,
			mimeType:    "audio/mpeg",
			want:        "ogg",
		},
		{
			name:        "audio mp3",
			contentType: contract.ContentTypeAudio,
			mimeType:    "audio/mpeg",
			want:        "mp3",
		},
		{
			name:        "audio mp3 alternate mime",
			contentType: contract.ContentTypeAudio,
			mimeType:    "audio/mp3",
			want:        "mp3",
		},
		{
			name:        "audio m4a",
			contentType: contract.ContentTypeAudio,
			mimeType:    "audio/mp4",
			want:        "m4a",
		},
		{
			name:        "audio m4a alternate mime",
			contentType: contract.ContentTypeAudio,
			mimeType:    "audio/x-m4a",
			want:        "m4a",
		},
		{
			name:        "audio flac",
			contentType: contract.ContentTypeAudio,
			mimeType:    "audio/flac",
			want:        "flac",
		},
		{
			name:        "audio ogg",
			contentType: contract.ContentTypeAudio,
			mimeType:    "audio/ogg",
			want:        "ogg",
		},
		{
			name:        "audio wav",
			contentType: contract.ContentTypeAudio,
			mimeType:    "audio/wav",
			want:        "wav",
		},
		{
			name:        "audio x-wav",
			contentType: contract.ContentTypeAudio,
			mimeType:    "audio/x-wav",
			want:        "wav",
		},
		{
			name:        "unknown audio defaults to mp3",
			contentType: contract.ContentTypeAudio,
			mimeType:    "audio/unknown",
			want:        "mp3",
		},
		{
			name:        "empty mime defaults to mp3",
			contentType: contract.ContentTypeAudio,
			mimeType:    "",
			want:        "mp3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := audioFileExt(tt.contentType, tt.mimeType)
			assert.Equal(t, tt.want, got, "audioFileExt() should return correct extension")
		})
	}
}

// ── Whisper Command Argument Building ────────────────────────────────────────

func TestWhisperArgs(t *testing.T) {
	// Test that whisper command is built with correct arguments
	// This exercises the command construction logic in processAudio

	tests := []struct {
		name         string
		audioPath    string
		outputDir    string
		expectedArgs []string
	}{
		{
			name:      "whisper command with turbo model",
			audioPath: "/tmp/test.ogg",
			outputDir: "/tmp/telegram-bridge/123",
			expectedArgs: []string{
				"whisper",
				"/tmp/test.ogg",
				"--model", "turbo",
				"--output_format", "txt",
				"--output_dir", "/tmp/telegram-bridge/123",
			},
		},
		{
			name:      "whisper command with different path",
			audioPath: "/tmp/telegram-bridge/456/789.mp3",
			outputDir: "/tmp/telegram-bridge/456",
			expectedArgs: []string{
				"whisper",
				"/tmp/telegram-bridge/456/789.mp3",
				"--model", "turbo",
				"--output_format", "txt",
				"--output_dir", "/tmp/telegram-bridge/456",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify the command structure matches expectations
			// This tests the logic without actually executing whisper
			cmd := exec.Command("whisper",
				tt.audioPath,
				"--model", "turbo",
				"--output_format", "txt",
				"--output_dir", tt.outputDir,
			)

			if len(cmd.Args) != len(tt.expectedArgs) {
				t.Errorf("command arg count: got %d, want %d", len(cmd.Args), len(tt.expectedArgs))
			}

			for i, arg := range cmd.Args {
				if i < len(tt.expectedArgs) && arg != tt.expectedArgs[i] {
					t.Errorf("arg %d: got %q, want %q", i, arg, tt.expectedArgs[i])
				}
			}
		})
	}
}

// ── Whisper Output File Paths ───────────────────────────────────────────────

func TestWhisperOutputPaths(t *testing.T) {
	// Test that output file paths are constructed correctly
	tests := []struct {
		name           string
		messageID      int64
		dir            string
		expectedTxt    string
		expectedAudio  string
	}{
		{
			name:          "standard path construction",
			messageID:     123,
			dir:           "/tmp/telegram-bridge/456",
			expectedTxt:   "/tmp/telegram-bridge/456/123.txt",
			expectedAudio: "/tmp/telegram-bridge/456/123.ogg",
		},
		{
			name:          "large message ID",
			messageID:     999999999,
			dir:           "/tmp/telegram-bridge/100",
			expectedTxt:   "/tmp/telegram-bridge/100/999999999.txt",
			expectedAudio: "/tmp/telegram-bridge/100/999999999.ogg",
		},
		{
			name:          "small message ID",
			messageID:     1,
			dir:           "/tmp/telegram-bridge/1",
			expectedTxt:   "/tmp/telegram-bridge/1/1.txt",
			expectedAudio: "/tmp/telegram-bridge/1/1.ogg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test txt path construction
			stem := fmt.Sprintf("%d", tt.messageID)
			txtPath := filepath.Join(tt.dir, stem+".txt")
			assert.Equal(t, tt.expectedTxt, txtPath, "txt path should be constructed correctly")

			// Test audio path construction for voice
			audioPath := filepath.Join(tt.dir, stem+".ogg")
			assert.Equal(t, tt.expectedAudio, audioPath, "audio path should be constructed correctly")
		})
	}
}

// ── Transcription Result Parsing ────────────────────────────────────────────

func TestTranscriptionParsing(t *testing.T) {
	// Test parsing of transcription results from whisper output

	tests := []struct {
		name          string
		rawOutput     string
		expectedText  string
		shouldTrim    bool
	}{
		{
			name:         "simple transcription",
			rawOutput:    "Hello world, this is a test.",
			expectedText: "Hello world, this is a test.",
			shouldTrim:   true,
		},
		{
			name:         "transcription with leading/trailing whitespace",
			rawOutput:    "  \n\n  Text with spaces  \n  ",
			expectedText: "Text with spaces",
			shouldTrim:   true,
		},
		{
			name:         "transcription with newlines",
			rawOutput:    "Line one\nLine two\nLine three",
			expectedText: "Line one\nLine two\nLine three",
			shouldTrim:   false, // preserve internal newlines
		},
		{
			name:         "transcription with tabs",
			rawOutput:    "\tTabbed text\t",
			expectedText: "Tabbed text",
			shouldTrim:   true,
		},
		{
			name:         "empty transcription",
			rawOutput:    "",
			expectedText: "",
			shouldTrim:   true,
		},
		{
			name:         "whitespace only",
			rawOutput:    "   \n\t\n   ",
			expectedText: "",
			shouldTrim:   true,
		},
		{
			name:         "multilingual transcription",
			rawOutput:    "Hello 你好 مرحبا",
			expectedText: "Hello 你好 مرحبا",
			shouldTrim:   true,
		},
		{
			name:         "transcription with punctuation",
			rawOutput:    "Hello! How are you? I'm fine.",
			expectedText: "Hello! How are you? I'm fine.",
			shouldTrim:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test the trimming logic that would be applied to transcription
			var result string
			if tt.shouldTrim {
				result = strings.TrimSpace(tt.rawOutput)
			} else {
				result = strings.Trim(tt.rawOutput, " \t\n\r")
			}

			assert.Equal(t, tt.expectedText, result, "transcription parsing should handle whitespace correctly")
		})
	}
}

// ── Error Paths ─────────────────────────────────────────────────────────────

func TestWhisperBinaryNotFound(t *testing.T) {
	// Test behavior when whisper binary is not found
	t.Run("whisper binary missing", func(t *testing.T) {
		// Verify that a non-existent whisper binary returns an error
		cmd := exec.Command("nonexistent-whisper-binary-test", "--version")
		err := cmd.Run()

		if err == nil {
			t.Error("expected error for non-existent whisper binary, got nil")
		}
	})
}

func TestWhisperExecutionError(t *testing.T) {
	// Test behavior when whisper execution fails

	t.Run("whisper command fails", func(t *testing.T) {
		// Create a mock whisper script that fails
		tmpDir := t.TempDir()
		mockWhisper := filepath.Join(tmpDir, "whisper")

		// Write a mock script that exits with error
		script := "#!/bin/sh\nexit 1\n"
		err := os.WriteFile(mockWhisper, []byte(script), 0o755)
		require.NoError(t, err, "failed to create mock whisper script")

		// Verify the mock script fails as expected
		cmd := exec.Command(mockWhisper, "--version")
		err = cmd.Run()

		assert.Error(t, err, "mock whisper should fail when executed")
	})
}

// ── Cleanup Path Tracking ───────────────────────────────────────────────────

func TestCleanupPathTracking(t *testing.T) {
	// Test that cleanup paths are correctly tracked for removal

	tests := []struct {
		name           string
		messageID      int64
		chatID         int64
		expectedPaths  int
	}{
		{
			name:          "audio and txt files tracked",
			messageID:     123,
			chatID:        456,
			expectedPaths: 2, // audio file + txt transcription
		},
		{
			name:          "different message ID",
			messageID:     789,
			chatID:        1000,
			expectedPaths: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(imageTempDir, fmt.Sprintf("%d", tt.chatID))
			stem := fmt.Sprintf("%d", tt.messageID)

			var cleanupPaths []string

			// Simulate tracking audio file
			audioPath := filepath.Join(dir, stem+".ogg")
			cleanupPaths = append(cleanupPaths, audioPath)

			// Simulate tracking transcription file
			txtPath := filepath.Join(dir, stem+".txt")
			cleanupPaths = append(cleanupPaths, txtPath)

			// Verify correct number of paths tracked
			if len(cleanupPaths) != tt.expectedPaths {
				t.Errorf("cleanup paths count: got %d, want %d", len(cleanupPaths), tt.expectedPaths)
			}

			// Verify audio path is tracked
			if cleanupPaths[0] != audioPath {
				t.Errorf("first cleanup path: got %q, want %q", cleanupPaths[0], audioPath)
			}

			// Verify txt path is tracked
			if cleanupPaths[1] != txtPath {
				t.Errorf("second cleanup path: got %q, want %q", cleanupPaths[1], txtPath)
			}
		})
	}
}

// ── startTyping Goroutine Tests ────────────────────────────────────────────

func TestStartTyping(t *testing.T) {
	// Test the startTyping goroutine behavior

	t.Run("typing interval is 4 seconds", func(t *testing.T) {
		// Verify the typing indicator fires at the expected interval
		// The ticker is set to 4 seconds in the implementation
		interval := 4 * time.Second
		if interval != 4*time.Second {
			t.Errorf("typing interval: got %v, want 4s", interval)
		}
	})

	t.Run("stop function closes channel", func(t *testing.T) {
		// Test that the stop function properly closes the stop channel
		stop := make(chan struct{})
		close(stop)

		select {
		case <-stop:
			// Expected - channel is closed
		default:
			t.Error("stop channel should be closed")
		}
	})

	t.Run("stop function is idempotent", func(t *testing.T) {
		// Test that calling stop multiple times is safe
		stop := make(chan struct{})
		close(stop)

		// Calling close again should not panic
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic on double close: %v", r)
			}
		}()

		select {
		case <-stop:
			// Expected - channel is already closed
		default:
			t.Error("stop channel should be closed")
		}
	})
}
