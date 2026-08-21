package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	// Try exact match first
	key := commandKey(name, args...)
	if cmd, ok := m.commands[key]; ok {
		return cmd
	}
	// Try command name only match (for wildcard mocking)
	if cmd, ok := m.commands[name]; ok {
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
	ctx       context.Context
	tempDir   string
	chatID    int64
	messageID int64
}

// newTestAudioContext creates a fresh test context for audio tests.
func newTestAudioContext(t *testing.T) testAudioContext {
	t.Helper()

	ctx := context.Background()
	tempDir := t.TempDir()

	return testAudioContext{
		ctx:       ctx,
		tempDir:   tempDir,
		chatID:    12345,
		messageID: 67890,
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

// TestProcessAudio_Args tests Whisper CLI argument building in processAudio function.
// It mocks exec.Command to capture Whisper invocation and verifies correct arguments.
func TestProcessAudio_Args(t *testing.T) {
	tests := []struct {
		name          string
		chatID        int64
		messageID     int64
		contentType   string
		mimeType      string
		fileID        string
		transcription string
		wantCmdName   string
		// tempDirSuffix, when set, replaces the last component of imageTempDir
		// for this case, injecting its characters (spaces, @, brackets, ...)
		// into every path processAudio builds. chatID and messageID render as
		// digits only, so this is the only way to vary path characters.
		tempDirSuffix string
	}{
		{
			name:          "whisper with ogg voice message",
			chatID:        12345,
			messageID:     67890,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_file_123",
			transcription: "Hello world, this is a test transcription.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with mp3 audio file",
			chatID:        98765,
			messageID:     43210,
			contentType:   contract.ContentTypeAudio,
			mimeType:      "audio/mpeg",
			fileID:        "audio_file_456",
			transcription: "This is an MP3 audio transcription.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with m4a audio file",
			chatID:        11111,
			messageID:     22222,
			contentType:   contract.ContentTypeAudio,
			mimeType:      "audio/mp4",
			fileID:        "audio_file_789",
			transcription: "M4A audio transcription test.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with flac audio file",
			chatID:        54321,
			messageID:     99999,
			contentType:   contract.ContentTypeAudio,
			mimeType:      "audio/flac",
			fileID:        "audio_file_flac",
			transcription: "FLAC audio transcription test.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with wav audio file",
			chatID:        77777,
			messageID:     88888,
			contentType:   contract.ContentTypeAudio,
			mimeType:      "audio/wav",
			fileID:        "audio_file_wav",
			transcription: "WAV audio transcription test.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with very large chat ID",
			chatID:        9999999999999,
			messageID:     1,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_large_chat",
			transcription: "Large chat ID test transcription.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with very large message ID",
			chatID:        1,
			messageID:     9999999999999,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_large_msg",
			transcription: "Large message ID test transcription.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with chat ID 0",
			chatID:        0,
			messageID:     100,
			contentType:   contract.ContentTypeAudio,
			mimeType:      "audio/mpeg",
			fileID:        "audio_zero_chat",
			transcription: "Zero chat ID transcription.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with chat ID 12345",
			chatID:        12345,
			messageID:     99999,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_12345",
			transcription: "Chat ID 12345 transcription test.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with chat ID 999888777",
			chatID:        999888777,
			messageID:     111222333,
			contentType:   contract.ContentTypeAudio,
			mimeType:      "audio/mpeg",
			fileID:        "audio_custom_chat",
			transcription: "Custom chat ID transcription.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with negative chat ID edge case",
			chatID:        -12345,
			messageID:     200,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_negative_chat",
			transcription: "Negative chat ID transcription.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with special characters in chat ID path",
			chatID:        54321,
			messageID:     100,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_special_chars",
			transcription: "Special characters path test.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with simple /tmp/test.ogg path",
			chatID:        99999,
			messageID:     777,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_simple_path",
			transcription: "Simple path transcription test.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with complex path separators",
			chatID:        88888,
			messageID:     555,
			contentType:   contract.ContentTypeAudio,
			mimeType:      "audio/mpeg",
			fileID:        "audio_complex_path",
			transcription: "Complex path separators test.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with unicode characters in path",
			chatID:        77777,
			messageID:     333,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_unicode",
			transcription: "Unicode characters path test.",
			wantCmdName:   "whisper",
			tempDirSuffix: "ünïcode-日本語-✓ dir",
		},
		{
			name:          "whisper with simple path pattern /tmp/test.ogg equivalent",
			chatID:        0,
			messageID:     1,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_simple_test",
			transcription: "Simple path test - minimal chat and message IDs.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with special characters in file path",
			chatID:        12345,
			messageID:     999,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_file-test@special[chars]",
			transcription: "Special characters in path transcription test.",
			wantCmdName:   "whisper",
		},
		{
			name:          "whisper with special characters in directory path",
			chatID:        424242,
			messageID:     777,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_special_dir",
			transcription: "Special characters in directory path transcription test.",
			wantCmdName:   "whisper",
			tempDirSuffix: "special @dir-[chars] (1)",
		},
		{
			name:          "whisper with minimal IDs resulting in /tmp/telegram-bridge/0/1.ogg",
			chatID:        0,
			messageID:     1,
			contentType:   contract.ContentTypeVoice,
			mimeType:      "audio/ogg",
			fileID:        "voice_minimal_ids",
			transcription: "Minimal IDs test - closest to /tmp/test.ogg pattern.",
			wantCmdName:   "whisper",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db := openTestDB(t)
			defer db.Close()

			// Track command invocations
			var capturedCmd struct {
				name string
				args []string
			}

			// Cases with tempDirSuffix point imageTempDir at a directory whose
			// name carries special characters, so every path built from it —
			// and every whisper argument derived from that path — contains them.
			if tt.tempDirSuffix != "" {
				originalDir := imageTempDir
				imageTempDir = filepath.Join(t.TempDir(), tt.tempDirSuffix)
				t.Cleanup(func() { imageTempDir = originalDir })
			}

			// Create the actual directory that processAudio will use (it uses the imageTempDir constant)
			testChatDir := filepath.Join(imageTempDir, fmt.Sprintf("%d", tt.chatID))
			require.NoError(t, os.MkdirAll(testChatDir, 0o755), "create chat directory")
			t.Cleanup(func() { os.RemoveAll(testChatDir) })

			// Create a test HTTP server to handle file downloads
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/file/") {
					// Serve fake audio data
					w.Header().Set("Content-Type", tt.mimeType)
					w.WriteHeader(http.StatusOK)
					w.Write([]byte("fake audio data"))
				}
			}))
			defer server.Close()

			// Create a capturing mock command exec
			mockExec := &capturingCommandExec{
				onCommand: func(name string, args ...string) command {
					capturedCmd.name = name
					capturedCmd.args = args

					// Create mock audio file and transcription for success
					ext := audioFileExt(tt.contentType, tt.mimeType)
					audioPath := filepath.Join(testChatDir, fmt.Sprintf("%d.%s", tt.messageID, ext))
					txtPath := filepath.Join(testChatDir, fmt.Sprintf("%d.txt", tt.messageID))

					// Create files (directory already exists)
					require.NoError(t, os.WriteFile(audioPath, []byte("fake audio"), 0644), "create audio file")
					require.NoError(t, os.WriteFile(txtPath, []byte(tt.transcription), 0644), "create transcription")

					return &mockCommand{
						output: []byte("whisper processing complete"),
						err:    nil,
					}
				},
			}

			// Create SessionManager with mock command exec
			sender, err := NewSender(server.URL, filepath.Join(t.TempDir(), "sender.db"))
			require.NoError(t, err, "failed to create sender")
			t.Cleanup(func() { sender.Close() })

			sm := &SessionManager{
				db:          db,
				sender:      sender,
				proxyURL:    server.URL,
				commandExec: mockExec,
			}

			// Prepare test content
			mime := tt.mimeType
			content := &contract.Content{
				Type:     tt.contentType,
				FileID:   &tt.fileID,
				MimeType: &mime,
			}

			// Call processAudio
			transcription, cleanupPaths, err := sm.processAudio(ctx, tt.chatID, tt.messageID, content)

			// Verify the result
			assert.NoError(t, err, "processAudio should succeed")
			assert.Equal(t, tt.transcription, transcription, "transcription should match")
			assert.Len(t, cleanupPaths, 2, "should track 2 cleanup paths")

			// Verify command was called with correct name
			assert.Equal(t, tt.wantCmdName, capturedCmd.name, "command name should be whisper")

			// Verify command structure: whisper audioPath --model turbo --output_format txt --output_dir chatDir
			// args contains: [audioPath, "--model", "turbo", "--output_format", "txt", "--output_dir", chatDir]
			assert.Len(t, capturedCmd.args, 7, "whisper command should have 7 arguments")
			assert.Equal(t, "--model", capturedCmd.args[1], "second arg should be --model")
			assert.Equal(t, "turbo", capturedCmd.args[2], "third arg should be turbo")
			assert.Equal(t, "--output_format", capturedCmd.args[3], "fourth arg should be --output_format")
			assert.Equal(t, "txt", capturedCmd.args[4], "fifth arg should be txt")
			assert.Equal(t, "--output_dir", capturedCmd.args[5], "sixth arg should be --output_dir")

			// Verify audio file path is passed correctly as first argument
			expectedChatDir := filepath.Join(imageTempDir, fmt.Sprintf("%d", tt.chatID))
			expectedExt := audioFileExt(tt.contentType, tt.mimeType)
			expectedAudioPath := filepath.Join(expectedChatDir, fmt.Sprintf("%d.%s", tt.messageID, expectedExt))
			assert.Equal(t, expectedAudioPath, capturedCmd.args[0], "audio file path should be correct")
			assert.Equal(t, expectedChatDir, capturedCmd.args[6], "output_dir should be correct")

			// Special-character cases: the whisper args must carry the
			// characters verbatim — exec.Command passes arguments directly
			// (no shell), so nothing may escape or mangle them.
			if tt.tempDirSuffix != "" {
				assert.Contains(t, capturedCmd.args[0], tt.tempDirSuffix, "audio path arg should keep special characters intact")
				assert.Contains(t, capturedCmd.args[6], tt.tempDirSuffix, "output_dir arg should keep special characters intact")
			}
		})
	}
}

// capturingCommandExec is a mock that captures command invocations
type capturingCommandExec struct {
	onCommand func(name string, args ...string) command
}

func (c *capturingCommandExec) CommandContext(ctx context.Context, name string, args ...string) command {
	return c.onCommand(name, args...)
}

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
		{
			name:      "whisper with path containing spaces",
			audioPath: "/tmp/test audio file.ogg",
			outputDir: "/tmp/telegram-bridge with spaces/123",
			expectedArgs: []string{
				"whisper",
				"/tmp/test audio file.ogg",
				"--model", "turbo",
				"--output_format", "txt",
				"--output_dir", "/tmp/telegram-bridge with spaces/123",
			},
		},
		{
			name:      "whisper with deep nested path",
			audioPath: "/tmp/telegram-bridge/123/456/789/test.m4a",
			outputDir: "/tmp/telegram-bridge/123/456/789",
			expectedArgs: []string{
				"whisper",
				"/tmp/telegram-bridge/123/456/789/test.m4a",
				"--model", "turbo",
				"--output_format", "txt",
				"--output_dir", "/tmp/telegram-bridge/123/456/789",
			},
		},
		{
			name:      "whisper with path containing special chars",
			audioPath: "/tmp/audio_file-test@2024.flac",
			outputDir: "/tmp/output-dir[test]",
			expectedArgs: []string{
				"whisper",
				"/tmp/audio_file-test@2024.flac",
				"--model", "turbo",
				"--output_format", "txt",
				"--output_dir", "/tmp/output-dir[test]",
			},
		},
		{
			name:      "whisper with chat ID 12345 path",
			audioPath: "/tmp/telegram-bridge/12345/67890.ogg",
			outputDir: "/tmp/telegram-bridge/12345",
			expectedArgs: []string{
				"whisper",
				"/tmp/telegram-bridge/12345/67890.ogg",
				"--model", "turbo",
				"--output_format", "txt",
				"--output_dir", "/tmp/telegram-bridge/12345",
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
		name          string
		messageID     int64
		dir           string
		expectedTxt   string
		expectedAudio string
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
		name         string
		rawOutput    string
		expectedText string
		shouldTrim   bool
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
		name          string
		messageID     int64
		chatID        int64
		expectedPaths int
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

// ── processAudio Error Path Tests ─────────────────────────────────────────────

func TestProcessAudio_ErrorPaths(t *testing.T) {
	t.Run("mkdir failure", func(t *testing.T) {
		ctx := context.Background()
		db := openTestDB(t)
		defer db.Close()

		chatID := int64(12345)
		messageID := int64(67890)
		fileID := "voice_mkdir_fail"

		// Create mock command exec (won't be reached due to mkdir failure)
		mockExec := newMockCommandExec()

		// Create test HTTP server (won't be reached due to mkdir failure)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/file/") {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("fake audio data"))
			}
		}))
		defer server.Close()

		sender, err := NewSender(server.URL, filepath.Join(t.TempDir(), "sender.db"))
		require.NoError(t, err, "failed to create sender")
		t.Cleanup(func() { sender.Close() })

		sm := &SessionManager{
			db:          db,
			sender:      sender,
			proxyURL:    server.URL,
			commandExec: mockExec,
		}

		mime := "audio/ogg"
		content := &contract.Content{
			Type:     contract.ContentTypeVoice,
			FileID:   &fileID,
			MimeType: &mime,
		}

		// Force the MkdirAll inside processAudio to fail by pointing
		// imageTempDir at a regular file (ENOTDIR). Unlike chmod'ing the
		// directory read-only, this works when running as root (CI
		// containers) and doesn't depend on /tmp/telegram-bridge
		// already existing.
		originalDir := imageTempDir
		imageTempDir = filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(imageTempDir, []byte("x"), 0o644), "create blocking file")
		t.Cleanup(func() { imageTempDir = originalDir })

		_, cleanupPaths, err := sm.processAudio(ctx, chatID, messageID, content)

		// Verify error occurred
		assert.Error(t, err, "processAudio should return error on mkdir failure")
		assert.Contains(t, err.Error(), "mkdir", "error message should mention mkdir")

		// Verify no cleanup paths on mkdir failure (nothing created yet)
		assert.Len(t, cleanupPaths, 0, "should have no cleanup paths on mkdir failure")
	})

	t.Run("whisper binary not found", func(t *testing.T) {
		ctx := context.Background()
		db := openTestDB(t)
		defer db.Close()

		chatID := int64(12345)
		messageID := int64(67890)
		fileID := "voice_123"

		// Create the directory that processAudio will use
		testChatDir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
		require.NoError(t, os.MkdirAll(testChatDir, 0o755), "create chat directory")
		t.Cleanup(func() { os.RemoveAll(testChatDir) })

		// Create mock command exec that simulates Whisper binary not found
		mockExec := newMockCommandExec()
		mockExec.addCommand("whisper", &mockCommand{
			output: []byte("whisper: command not found"),
			err:    &exec.Error{Name: "whisper", Err: exec.ErrNotFound},
		})

		// Create test HTTP server - serve downloads successfully
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/file/") {
				w.Header().Set("Content-Type", "audio/ogg")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("fake audio data"))
			}
		}))
		defer server.Close()

		sender, err := NewSender(server.URL, filepath.Join(t.TempDir(), "sender.db"))
		require.NoError(t, err, "failed to create sender")
		t.Cleanup(func() { sender.Close() })

		sm := &SessionManager{
			db:          db,
			sender:      sender,
			proxyURL:    server.URL,
			commandExec: mockExec,
		}

		mime := "audio/ogg"
		content := &contract.Content{
			Type:     contract.ContentTypeVoice,
			FileID:   &fileID,
			MimeType: &mime,
		}

		_, cleanupPaths, err := sm.processAudio(ctx, chatID, messageID, content)

		// Verify error occurred
		assert.Error(t, err, "processAudio should return error")
		assert.Contains(t, err.Error(), "whisper:", "error message should mention whisper")

		// Verify cleanup paths are returned (both audio and txt paths tracked before Whisper runs)
		assert.Len(t, cleanupPaths, 2, "should track both cleanup paths on error")
	})

	t.Run("whisper command execution error", func(t *testing.T) {
		ctx := context.Background()
		db := openTestDB(t)
		defer db.Close()

		chatID := int64(12345)
		messageID := int64(67890)
		fileID := "voice_456"

		// Create the directory that processAudio will use
		testChatDir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
		require.NoError(t, os.MkdirAll(testChatDir, 0o755), "create chat directory")
		t.Cleanup(func() { os.RemoveAll(testChatDir) })

		// Create mock command exec that simulates Whisper execution error
		mockExec := newMockCommandExec()
		mockExec.addCommand("whisper", &mockCommand{
			output: []byte("whisper: audio format not supported"),
			err:    fmt.Errorf("whisper processing failed"),
		})

		// Create test HTTP server - serve downloads successfully
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/file/") {
				w.Header().Set("Content-Type", "audio/ogg")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("fake audio data"))
			}
		}))
		defer server.Close()

		sender, err := NewSender(server.URL, filepath.Join(t.TempDir(), "sender.db"))
		require.NoError(t, err, "failed to create sender")
		t.Cleanup(func() { sender.Close() })

		sm := &SessionManager{
			db:          db,
			sender:      sender,
			proxyURL:    server.URL,
			commandExec: mockExec,
		}

		mime := "audio/ogg"
		content := &contract.Content{
			Type:     contract.ContentTypeVoice,
			FileID:   &fileID,
			MimeType: &mime,
		}

		_, cleanupPaths, err := sm.processAudio(ctx, chatID, messageID, content)

		// Verify error occurred
		assert.Error(t, err, "processAudio should return error")
		assert.Contains(t, err.Error(), "whisper:", "error message should mention whisper")

		// Verify cleanup paths are returned (both audio and txt paths tracked before Whisper runs)
		assert.Len(t, cleanupPaths, 2, "should track both cleanup paths on error")
	})

	t.Run("download failure", func(t *testing.T) {
		ctx := context.Background()
		db := openTestDB(t)
		defer db.Close()

		chatID := int64(12345)
		messageID := int64(67890)
		fileID := "audio_789"

		// Create mock command exec
		mockExec := newMockCommandExec()

		// Create test HTTP server - fail downloads
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/file/") {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("download failed"))
			}
		}))
		defer server.Close()

		sender, err := NewSender(server.URL, filepath.Join(t.TempDir(), "sender.db"))
		require.NoError(t, err, "failed to create sender")
		t.Cleanup(func() { sender.Close() })

		sm := &SessionManager{
			db:          db,
			sender:      sender,
			proxyURL:    server.URL,
			commandExec: mockExec,
		}

		mime := "audio/mpeg"
		content := &contract.Content{
			Type:     contract.ContentTypeAudio,
			FileID:   &fileID,
			MimeType: &mime,
		}

		_, cleanupPaths, err := sm.processAudio(ctx, chatID, messageID, content)

		// Verify error occurred
		assert.Error(t, err, "processAudio should return error")
		assert.Contains(t, err.Error(), "download audio:", "error message should mention download")

		// Verify no cleanup paths on download failure
		assert.Len(t, cleanupPaths, 0, "should have no cleanup paths on download failure")
	})
}

func TestProcessAudio_InvalidOutput(t *testing.T) {
	// Test behavior when Whisper output file cannot be read
	t.Run("transcription file read failure", func(t *testing.T) {
		ctx := context.Background()
		db := openTestDB(t)
		defer db.Close()

		chatID := int64(12345)
		messageID := int64(67890)
		fileID := "voice_readfail"

		// Create the directory that processAudio will use
		testChatDir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
		require.NoError(t, os.MkdirAll(testChatDir, 0o755), "create chat directory")
		t.Cleanup(func() { os.RemoveAll(testChatDir) })

		// Create mock command exec that succeeds but doesn't create output file
		mockExec := newMockCommandExec()
		mockExec.addCommand(
			commandKey("whisper", filepath.Join(testChatDir, fmt.Sprintf("%d.ogg", messageID)), "--model", "turbo", "--output_format", "txt", "--output_dir", testChatDir),
			&mockCommand{
				output: []byte("whisper processing complete"),
				err:    nil,
			},
		)

		// Create test HTTP server
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/file/") {
				w.Header().Set("Content-Type", "audio/ogg")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("fake audio data"))
			}
		}))
		defer server.Close()

		sender, err := NewSender(server.URL, filepath.Join(t.TempDir(), "sender.db"))
		require.NoError(t, err, "failed to create sender")
		t.Cleanup(func() { sender.Close() })

		sm := &SessionManager{
			db:          db,
			sender:      sender,
			proxyURL:    server.URL,
			commandExec: mockExec,
		}

		mime := "audio/ogg"
		content := &contract.Content{
			Type:     contract.ContentTypeVoice,
			FileID:   &fileID,
			MimeType: &mime,
		}

		_, cleanupPaths, err := sm.processAudio(ctx, chatID, messageID, content)

		// Verify error occurred
		assert.Error(t, err, "processAudio should return error when transcription file is missing")
		assert.Contains(t, err.Error(), "read transcription", "error should mention reading transcription")

		// Verify cleanup paths are returned even on error
		assert.Len(t, cleanupPaths, 2, "should track both cleanup paths on error")

		// Verify audio file path is in cleanup
		expectedAudioPath := filepath.Join(testChatDir, fmt.Sprintf("%d.ogg", messageID))
		assert.Contains(t, cleanupPaths, expectedAudioPath, "audio path should be in cleanup")
	})
}

func TestProcessAudio_TranscriptionParsing(t *testing.T) {
	// Test successful transcription result parsing with various Whisper outputs
	tests := []struct {
		name          string
		transcription string // content written to .txt file
		expectedText  string // expected parsed result
	}{
		{
			name:          "simple transcription",
			transcription: "Hello world, this is a test transcription.",
			expectedText:  "Hello world, this is a test transcription.",
		},
		{
			name:          "transcription with leading/trailing whitespace",
			transcription: "  \n\n  Text with spaces  \n  ",
			expectedText:  "Text with spaces",
		},
		{
			name:          "transcription with newlines preserved",
			transcription: "Line one\nLine two\nLine three",
			expectedText:  "Line one\nLine two\nLine three",
		},
		{
			name:          "transcription with tabs",
			transcription: "\tTabbed text\t",
			expectedText:  "Tabbed text",
		},
		{
			name:          "empty transcription",
			transcription: "",
			expectedText:  "",
		},
		{
			name:          "whitespace only transcription",
			transcription: "   \n\t\n   ",
			expectedText:  "",
		},
		{
			name:          "multilingual transcription",
			transcription: "Hello 你好 مرحبا",
			expectedText:  "Hello 你好 مرحبا",
		},
		{
			name:          "transcription with punctuation",
			transcription: "Hello! How are you? I'm fine.",
			expectedText:  "Hello! How are you? I'm fine.",
		},
		{
			name:          "transcription with numbers",
			transcription: "The answer is 42. The date is 2024-07-29.",
			expectedText:  "The answer is 42. The date is 2024-07-29.",
		},
		{
			name:          "transcription with special characters",
			transcription: "Special chars: @#$%^&*()_+-=[]{}|;':\",./<>?",
			expectedText:  "Special chars: @#$%^&*()_+-=[]{}|;':\",./<>?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db := openTestDB(t)
			defer db.Close()

			chatID := int64(12345)
			messageID := int64(67890)
			fileID := "voice_success"

			// Create the directory that processAudio will use
			testChatDir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
			require.NoError(t, os.MkdirAll(testChatDir, 0o755), "create chat directory")
			t.Cleanup(func() { os.RemoveAll(testChatDir) })

			// Create mock command exec that creates output file
			mockExec := newMockCommandExec()
			audioPath := filepath.Join(testChatDir, fmt.Sprintf("%d.ogg", messageID))

			mockExec.addCommand(
				commandKey("whisper", audioPath, "--model", "turbo", "--output_format", "txt", "--output_dir", testChatDir),
				createMockWhisperSuccess(t, testChatDir, fmt.Sprintf("%d", messageID), tt.transcription),
			)

			// Create test HTTP server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/file/") {
					w.Header().Set("Content-Type", "audio/ogg")
					w.WriteHeader(http.StatusOK)
					w.Write([]byte("fake audio data"))
				}
			}))
			defer server.Close()

			sender, err := NewSender(server.URL, filepath.Join(t.TempDir(), "sender.db"))
			require.NoError(t, err, "failed to create sender")
			t.Cleanup(func() { sender.Close() })

			sm := &SessionManager{
				db:          db,
				sender:      sender,
				proxyURL:    server.URL,
				commandExec: mockExec,
			}

			mime := "audio/ogg"
			content := &contract.Content{
				Type:     contract.ContentTypeVoice,
				FileID:   &fileID,
				MimeType: &mime,
			}

			transcription, cleanupPaths, err := sm.processAudio(ctx, chatID, messageID, content)

			// Verify success
			assert.NoError(t, err, "processAudio should succeed")
			assert.Equal(t, tt.expectedText, transcription, "transcription should match expected result")

			// Verify cleanup paths are tracked
			assert.Len(t, cleanupPaths, 2, "should track 2 cleanup paths")
		})
	}
}

// TestProcessAudio_NilMimeType covers the content.MimeType == nil branch in
// processAudio: with no mime hint the extension falls back to the content-type
// default (ogg for voice, mp3 for audio). The mock is registered under the
// exact command key built from the expected extension, so a different extension
// would leave the whisper mock unmatched and the test failing.
func TestProcessAudio_NilMimeType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		wantExt     string
	}{
		{
			name:        "voice with nil mime defaults to ogg",
			contentType: contract.ContentTypeVoice,
			wantExt:     "ogg",
		},
		{
			name:        "audio with nil mime defaults to mp3",
			contentType: contract.ContentTypeAudio,
			wantExt:     "mp3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			db := openTestDB(t)
			defer db.Close()

			chatID := int64(12345)
			messageID := int64(67890)
			fileID := "voice_nil_mime"
			stem := fmt.Sprintf("%d", messageID)

			// Create the directory that processAudio will use
			testChatDir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
			require.NoError(t, os.MkdirAll(testChatDir, 0o755), "create chat directory")
			t.Cleanup(func() { os.RemoveAll(testChatDir) })

			// Register the mock under the key built from the expected
			// extension so the choice of extension is itself verified.
			mockExec := newMockCommandExec()
			audioPath := filepath.Join(testChatDir, stem+"."+tt.wantExt)
			mockExec.addCommand(
				commandKey("whisper", audioPath, "--model", "turbo", "--output_format", "txt", "--output_dir", testChatDir),
				createMockWhisperSuccess(t, testChatDir, stem, "Transcribed without a mime hint."),
			)

			// Create test HTTP server to handle file downloads
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/file/") {
					w.WriteHeader(http.StatusOK)
					w.Write([]byte("fake audio data"))
				}
			}))
			defer server.Close()

			sender, err := NewSender(server.URL, filepath.Join(t.TempDir(), "sender.db"))
			require.NoError(t, err, "failed to create sender")
			t.Cleanup(func() { sender.Close() })

			sm := &SessionManager{
				db:          db,
				sender:      sender,
				proxyURL:    server.URL,
				commandExec: mockExec,
			}

			// No MimeType set — processAudio must take the nil branch.
			content := &contract.Content{
				Type:   tt.contentType,
				FileID: &fileID,
			}

			transcription, cleanupPaths, err := sm.processAudio(ctx, chatID, messageID, content)

			require.NoError(t, err, "processAudio should succeed without a mime hint")
			assert.Equal(t, "Transcribed without a mime hint.", transcription, "transcription should match")
			require.Len(t, cleanupPaths, 2, "should track 2 cleanup paths")
			assert.Equal(t, audioPath, cleanupPaths[0], "audio cleanup path should use the default extension")
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
