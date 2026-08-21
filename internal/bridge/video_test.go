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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Test Helpers ─────────────────────────────────────────────────────────────

// useVideoTempDir points imageTempDir at a fresh temp directory for the
// duration of the test, restoring the original value afterwards. Video tests
// must never touch the real /tmp/telegram-bridge.
func useVideoTempDir(t *testing.T) string {
	t.Helper()
	original := imageTempDir
	imageTempDir = t.TempDir()
	t.Cleanup(func() { imageTempDir = original })
	return imageTempDir
}

// newVideoSessionManager returns a SessionManager wired for video tests:
// processVideo only touches proxyURL and commandExec, so no db or sender
// is needed.
func newVideoSessionManager(proxyURL string, exec commandExec) *SessionManager {
	return &SessionManager{
		proxyURL:    proxyURL,
		commandExec: exec,
	}
}

// recordedCommand is a single external command invocation captured by
// videoRecordingExec.
type recordedCommand struct {
	name string
	args []string
}

// videoRecordingExec is a commandExec mock for the full video pipeline. It
// records every invocation and simulates the filesystem side effects that the
// real tools would produce:
//
//   - ffmpeg keyframe call (has "-vf"): writes frameCount
//     {stem}_frame_%04d.jpg files next to the output pattern
//   - ffmpeg audio call (has "-vn"): writes the output wav
//   - whisper: writes {stem}_audio.txt into --output_dir
//
// The err fields inject failures at each stage; skipTxt simulates a whisper
// run that succeeds without producing its output file.
type videoRecordingExec struct {
	mu          sync.Mutex
	invocations []recordedCommand

	frameCount int    // frames created by the keyframes ffmpeg call
	transcript string // content whisper writes to its txt output

	keyframesErr error // returned by the keyframes ffmpeg call
	audioErr     error // returned by the audio ffmpeg call
	whisperErr   error // returned by the whisper call
	skipTxt      bool  // whisper succeeds but writes no txt file
}

func (v *videoRecordingExec) CommandContext(ctx context.Context, name string, args ...string) command {
	v.mu.Lock()
	v.invocations = append(v.invocations, recordedCommand{name: name, args: args})
	v.mu.Unlock()

	switch name {
	case "ffmpeg":
		if containsArg(args, "-vf") {
			if v.keyframesErr != nil {
				return &mockCommand{output: []byte("ffmpeg keyframes failed"), err: v.keyframesErr}
			}
			v.writeFrames(args)
			return &mockCommand{output: []byte("ffmpeg ok")}
		}
		if v.audioErr != nil {
			return &mockCommand{output: []byte("ffmpeg audio failed"), err: v.audioErr}
		}
		// Audio extraction: last arg is the output wav path.
		if len(args) > 0 {
			_ = os.WriteFile(args[len(args)-1], []byte("RIFF fake wav"), 0o644)
		}
		return &mockCommand{output: []byte("ffmpeg ok")}
	case "whisper":
		if v.whisperErr != nil {
			return &mockCommand{output: []byte("whisper failed"), err: v.whisperErr}
		}
		if !v.skipTxt {
			v.writeTranscript(args)
		}
		return &mockCommand{output: []byte("whisper ok")}
	}
	return &mockCommand{err: fmt.Errorf("unexpected command: %s", name)}
}

// writeFrames derives the frame directory and stem from the output pattern
// (last arg of the keyframes call) and writes frameCount frame files.
func (v *videoRecordingExec) writeFrames(args []string) {
	pattern := args[len(args)-1]
	dir := filepath.Dir(pattern)
	base := filepath.Base(pattern)              // {stem}_frame_%04d.jpg
	stem := strings.TrimSuffix(base, "_frame_%04d.jpg")
	for i := 1; i <= v.frameCount; i++ {
		frame := filepath.Join(dir, fmt.Sprintf("%s_frame_%04d.jpg", stem, i))
		_ = os.WriteFile(frame, []byte("fake jpeg"), 0o644)
	}
}

// writeTranscript derives dir and stem from the whisper arguments
// (audio path is args[0], --output_dir precedes the dir) and writes the txt
// file the real whisper CLI would produce for a {stem}_audio.wav input.
func (v *videoRecordingExec) writeTranscript(args []string) {
	dir := ""
	for i, a := range args {
		if a == "--output_dir" && i+1 < len(args) {
			dir = args[i+1]
		}
	}
	stem := strings.TrimSuffix(filepath.Base(args[0]), "_audio.wav")
	txt := filepath.Join(dir, stem+"_audio.txt")
	_ = os.WriteFile(txt, []byte(v.transcript), 0o644)
}

func (v *videoRecordingExec) commands() []recordedCommand {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]recordedCommand(nil), v.invocations...)
}

// findInvocation returns the first recorded command with the given name and
// marker argument (e.g. "-vf" for the keyframes ffmpeg call).
func findInvocation(cmds []recordedCommand, name, marker string) *recordedCommand {
	for i := range cmds {
		if cmds[i].name == name && containsArg(cmds[i].args, marker) {
			return &cmds[i]
		}
	}
	return nil
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// ── Resize Bounds / Filter Construction ──────────────────────────────────────

// TestVideoKeyframeFilter verifies the ffmpeg video-filter argument: the
// frame rate, the fixed output width, and the aspect-preserving height (-1
// tells ffmpeg to compute it). This is the entire resize policy for
// keyframes, so it is pinned exactly.
func TestVideoKeyframeFilter(t *testing.T) {
	filter := keyframeFilter()

	assert.Equal(t, "fps=0.5,scale=800:-1", filter,
		"filter must sample every 2s, scale to 800px wide, height computed by ffmpeg")
	assert.True(t, strings.HasPrefix(filter, "fps=0.5,"), "fps must come first")
	assert.Contains(t, filter, "scale=800:-1", "width fixed at 800, height -1 preserves aspect ratio")
}

// ── extractKeyframes ─────────────────────────────────────────────────────────

// TestVideoExtractKeyframes_Args runs extractKeyframes with a recording mock
// and verifies the exact ffmpeg invocation: input, filter, frame cap,
// overwrite flag, and output pattern. Frame collection via glob is verified
// against files the mock writes, in glob (sorted) order.
func TestVideoExtractKeyframes_Args(t *testing.T) {
	tests := []struct {
		name       string
		dir        string
		stem       string
		frameCount int
	}{
		{name: "standard ids", dir: filepath.Join(t.TempDir(), "12345"), stem: "67890", frameCount: 3},
		{name: "minimal ids", dir: filepath.Join(t.TempDir(), "0"), stem: "1", frameCount: 1},
		{name: "large ids", dir: filepath.Join(t.TempDir(), "999888777"), stem: "9999999999999", frameCount: 10},
		{name: "special characters in dir", dir: filepath.Join(t.TempDir(), "special @dir-[chars] (1)"), stem: "42", frameCount: 2},
		{name: "unicode dir", dir: filepath.Join(t.TempDir(), "ünïcode-日本語 dir"), stem: "7", frameCount: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.MkdirAll(tt.dir, 0o755), "create extraction dir")
			videoPath := filepath.Join(tt.dir, tt.stem+".mp4")
			require.NoError(t, os.WriteFile(videoPath, []byte("fake video"), 0o644), "create input video")

			rec := &videoRecordingExec{frameCount: tt.frameCount}
			sm := newVideoSessionManager("", rec)

			got, err := sm.extractKeyframes(context.Background(), videoPath, tt.dir, tt.stem)
			require.NoError(t, err, "extractKeyframes should succeed")

			// Exactly one ffmpeg invocation with the expected args.
			cmds := rec.commands()
			require.Len(t, cmds, 1, "extractKeyframes should invoke ffmpeg once")
			assert.Equal(t, "ffmpeg", cmds[0].name, "command must be ffmpeg")
			assert.Equal(t, []string{
				"-i", videoPath,
				"-vf", "fps=0.5,scale=800:-1",
				"-frames:v", "10",
				"-y",
				filepath.Join(tt.dir, tt.stem+"_frame_%04d.jpg"),
			}, cmds[0].args, "ffmpeg args must match exactly")

			// The returned paths are the globbed frame files, sorted.
			var want []string
			for i := 1; i <= tt.frameCount; i++ {
				want = append(want, filepath.Join(tt.dir, fmt.Sprintf("%s_frame_%04d.jpg", tt.stem, i)))
			}
			assert.Equal(t, want, got, "returned frames must be the sorted glob matches")
		})
	}
}

// TestVideoExtractKeyframes_NoFrames covers a video too short (or with no
// visual content) for which ffmpeg writes no frames: success with an empty
// (non-nil) slice, not an error.
func TestVideoExtractKeyframes_NoFrames(t *testing.T) {
	dir := filepath.Join(useVideoTempDir(t), "12345")
	require.NoError(t, os.MkdirAll(dir, 0o755), "create extraction dir")
	videoPath := filepath.Join(dir, "67890.mp4")
	require.NoError(t, os.WriteFile(videoPath, []byte("fake video"), 0o644), "create input video")

	rec := &videoRecordingExec{frameCount: 0}
	sm := newVideoSessionManager("", rec)

	got, err := sm.extractKeyframes(context.Background(), videoPath, dir, "67890")
	require.NoError(t, err, "no frames is not an error")
	require.NotNil(t, got, "result must be an empty slice, not nil")
	assert.Empty(t, got, "no frames extracted")
}

// TestVideoExtractKeyframes_Errors covers the ffmpeg failure paths: the
// binary missing from PATH entirely, and ffmpeg exiting non-zero with output
// that must surface in the error message.
func TestVideoExtractKeyframes_Errors(t *testing.T) {
	dir := filepath.Join(useVideoTempDir(t), "12345")
	require.NoError(t, os.MkdirAll(dir, 0o755), "create extraction dir")
	videoPath := filepath.Join(dir, "67890.mp4")
	require.NoError(t, os.WriteFile(videoPath, []byte("fake video"), 0o644), "create input video")

	t.Run("ffmpeg binary not found", func(t *testing.T) {
		mockExec := newMockCommandExec()
		mockExec.addCommand("ffmpeg", &mockCommand{
			output: nil,
			err:    &exec.Error{Name: "ffmpeg", Err: exec.ErrNotFound},
		})
		sm := newVideoSessionManager("", mockExec)

		got, err := sm.extractKeyframes(context.Background(), videoPath, dir, "67890")
		require.Error(t, err, "missing ffmpeg binary must fail")
		assert.Contains(t, err.Error(), "ffmpeg:", "error should mention ffmpeg")
		assert.Contains(t, err.Error(), "executable file not found", "error should carry exec's not-found detail")
		assert.Nil(t, got, "no frame paths on error")
	})

	t.Run("ffmpeg execution failure includes output", func(t *testing.T) {
		mockExec := newMockCommandExec()
		mockExec.addCommand("ffmpeg", &mockCommand{
			output: []byte("Invalid data found when processing input"),
			err:    fmt.Errorf("exit status 1"),
		})
		sm := newVideoSessionManager("", mockExec)

		got, err := sm.extractKeyframes(context.Background(), videoPath, dir, "67890")
		require.Error(t, err, "ffmpeg failure must propagate")
		assert.Contains(t, err.Error(), "ffmpeg:", "error should mention ffmpeg")
		assert.Contains(t, err.Error(), "Invalid data found when processing input", "ffmpeg output must be included for diagnosis")
		assert.Nil(t, got, "no frame paths on error")
	})
}

// ── extractAudio ─────────────────────────────────────────────────────────────

// TestVideoExtractAudio verifies the ffmpeg audio-extraction invocation: no
// video stream, 16-bit little-endian PCM for whisper, overwrite, and the
// exact output path.
func TestVideoExtractAudio(t *testing.T) {
	dir := filepath.Join(useVideoTempDir(t), "12345")
	require.NoError(t, os.MkdirAll(dir, 0o755), "create extraction dir")
	videoPath := filepath.Join(dir, "67890.mp4")
	audioPath := filepath.Join(dir, "67890_audio.wav")

	t.Run("args", func(t *testing.T) {
		rec := &videoRecordingExec{}
		sm := newVideoSessionManager("", rec)

		err := sm.extractAudio(context.Background(), videoPath, audioPath)
		require.NoError(t, err, "extractAudio should succeed")

		cmds := rec.commands()
		require.Len(t, cmds, 1, "extractAudio should invoke ffmpeg once")
		assert.Equal(t, "ffmpeg", cmds[0].name, "command must be ffmpeg")
		assert.Equal(t, []string{
			"-i", videoPath,
			"-vn",
			"-acodec", "pcm_s16le",
			"-y",
			audioPath,
		}, cmds[0].args, "audio extraction args must match exactly")
	})

	t.Run("ffmpeg error", func(t *testing.T) {
		mockExec := newMockCommandExec()
		mockExec.addCommand("ffmpeg", &mockCommand{
			output: []byte("corrupt stream"),
			err:    fmt.Errorf("exit status 1"),
		})
		sm := newVideoSessionManager("", mockExec)

		err := sm.extractAudio(context.Background(), videoPath, audioPath)
		require.Error(t, err, "ffmpeg failure must propagate")
		assert.Contains(t, err.Error(), "ffmpeg:", "error should mention ffmpeg")
		assert.Contains(t, err.Error(), "corrupt stream", "ffmpeg output must be included")
	})

	t.Run("ffmpeg binary not found", func(t *testing.T) {
		mockExec := newMockCommandExec()
		mockExec.addCommand("ffmpeg", &mockCommand{
			err: &exec.Error{Name: "ffmpeg", Err: exec.ErrNotFound},
		})
		sm := newVideoSessionManager("", mockExec)

		err := sm.extractAudio(context.Background(), videoPath, audioPath)
		require.Error(t, err, "missing ffmpeg binary must fail")
		assert.Contains(t, err.Error(), "ffmpeg:", "error should mention ffmpeg")
	})
}

// ── transcribeAudio ──────────────────────────────────────────────────────────

// TestVideoTranscribeAudio verifies the whisper invocation for the extracted
// audio track, the trimming of the transcription read back from its txt
// output, and the failure paths (whisper error, missing txt output).
func TestVideoTranscribeAudio(t *testing.T) {
	useVideoTempDir(t)

	// Each subtest gets a fresh dir: the "txt missing" case must not find a
	// transcription left behind by an earlier subtest.
	newDir := func(t *testing.T) (dir, audioPath string) {
		t.Helper()
		dir = t.TempDir()
		return dir, filepath.Join(dir, "67890_audio.wav")
	}

	t.Run("args and trimmed transcript", func(t *testing.T) {
		dir, audioPath := newDir(t)
		rec := &videoRecordingExec{transcript: "  hello from the audio track  \n"}
		sm := newVideoSessionManager("", rec)

		got, err := sm.transcribeAudio(context.Background(), audioPath, dir, "67890")
		require.NoError(t, err, "transcribeAudio should succeed")
		assert.Equal(t, "hello from the audio track", got, "transcription must be trimmed")

		cmds := rec.commands()
		require.Len(t, cmds, 1, "transcribeAudio should invoke whisper once")
		assert.Equal(t, "whisper", cmds[0].name, "command must be whisper")
		assert.Equal(t, []string{
			audioPath,
			"--model", "turbo",
			"--output_format", "txt",
			"--output_dir", dir,
		}, cmds[0].args, "whisper args must match exactly")
	})

	t.Run("whisper error", func(t *testing.T) {
		dir, audioPath := newDir(t)
		mockExec := newMockCommandExec()
		mockExec.addCommand("whisper", &mockCommand{
			output: []byte("whisper: CUDA error"),
			err:    fmt.Errorf("exit status 1"),
		})
		sm := newVideoSessionManager("", mockExec)

		got, err := sm.transcribeAudio(context.Background(), audioPath, dir, "67890")
		require.Error(t, err, "whisper failure must propagate")
		assert.Contains(t, err.Error(), "whisper:", "error should mention whisper")
		assert.Contains(t, err.Error(), "CUDA error", "whisper output must be included")
		assert.Empty(t, got, "no transcription on error")
	})

	t.Run("whisper succeeds but txt missing", func(t *testing.T) {
		dir, audioPath := newDir(t)
		rec := &videoRecordingExec{skipTxt: true}
		sm := newVideoSessionManager("", rec)

		got, err := sm.transcribeAudio(context.Background(), audioPath, dir, "67890")
		require.Error(t, err, "missing transcription file must fail")
		assert.Contains(t, err.Error(), "read transcription", "error should mention reading the transcription")
		assert.Empty(t, got, "no transcription on error")
	})
}

// ── processVideo Full Pipeline ───────────────────────────────────────────────

// TestVideoProcessVideo drives the full pipeline (download → keyframes →
// audio extraction → transcription) with mocked ffmpeg/whisper. Both video
// content types that route to processVideo (ContentTypeVideo and
// ContentTypeVideoNote, see session_manager.go) land here, so the table
// covers both.
func TestVideoProcessVideo(t *testing.T) {
	tests := []struct {
		name        string
		chatID      int64
		messageID   int64
		contentType string
		frameCount  int
		transcript  string
		// tempDirSuffix, when set, replaces imageTempDir with a directory
		// whose name carries the suffix, injecting those characters into
		// every path processVideo builds. IDs render as digits only, so
		// this is the only way to vary path characters.
		tempDirSuffix string
	}{
		{
			name:        "video message with keyframes and transcription",
			chatID:      12345,
			messageID:   67890,
			contentType: "video",
			frameCount:  3,
			transcript:  "Hello from the video audio track.",
		},
		{
			name:        "video note routes through the same pipeline",
			chatID:      99,
			messageID:   1,
			contentType: "video_note",
			frameCount:  2,
			transcript:  "Round video note transcription.",
		},
		{
			name:        "video with no extractable frames",
			chatID:      555,
			messageID:   666,
			contentType: "video",
			frameCount:  0,
			transcript:  "Audio-only content.",
		},
		{
			name:        "large chat and message ids",
			chatID:      999888777,
			messageID:   9999999999999,
			contentType: "video",
			frameCount:  10,
			transcript:  "Capped frame count.",
		},
		{
			name:        "special characters in temp dir path",
			chatID:      424242,
			messageID:   777,
			contentType: "video",
			frameCount:  2,
			transcript:  "Special character path test.",
			tempDirSuffix: "special @dir-[chars] (1)",
		},
		{
			name:        "unicode temp dir path",
			chatID:      77777,
			messageID:   333,
			contentType: "video_note",
			frameCount:  1,
			transcript:  "Unicode path test.",
			tempDirSuffix: "ünïcode-日本語 dir",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			tempRoot := useVideoTempDir(t)
			if tt.tempDirSuffix != "" {
				original := imageTempDir
				imageTempDir = filepath.Join(t.TempDir(), tt.tempDirSuffix)
				t.Cleanup(func() { imageTempDir = original })
				tempRoot = imageTempDir
			}

			// The proxy serves the video download.
			var downloadCount int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/file/") {
					downloadCount++
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("fake mp4 data"))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()

			rec := &videoRecordingExec{frameCount: tt.frameCount, transcript: tt.transcript}
			sm := newVideoSessionManager(server.URL, rec)

			fileID := fmt.Sprintf("%s_file_%d", tt.contentType, tt.messageID)
			result, cleanupPaths, err := sm.processVideo(ctx, tt.chatID, tt.messageID, fileID)
			require.NoError(t, err, "processVideo should succeed")
			require.NotNil(t, result, "result must be non-nil on success")
			assert.Equal(t, 1, downloadCount, "video should be downloaded exactly once")

			dir := filepath.Join(tempRoot, fmt.Sprintf("%d", tt.chatID))
			stem := fmt.Sprintf("%d", tt.messageID)
			videoPath := filepath.Join(dir, stem+".mp4")
			audioPath := filepath.Join(dir, stem+"_audio.wav")

			// All three pipeline commands ran, with the right args.
			cmds := rec.commands()
			require.Len(t, cmds, 3, "pipeline must run keyframes, audio extraction, and whisper")

			keyframes := findInvocation(cmds, "ffmpeg", "-vf")
			require.NotNil(t, keyframes, "keyframes ffmpeg call missing")
			assert.Equal(t, []string{
				"-i", videoPath,
				"-vf", "fps=0.5,scale=800:-1",
				"-frames:v", "10",
				"-y",
				filepath.Join(dir, stem+"_frame_%04d.jpg"),
			}, keyframes.args, "keyframes invocation must match")

			audioExtraction := findInvocation(cmds, "ffmpeg", "-vn")
			require.NotNil(t, audioExtraction, "audio extraction ffmpeg call missing")
			assert.Equal(t, []string{
				"-i", videoPath,
				"-vn",
				"-acodec", "pcm_s16le",
				"-y",
				audioPath,
			}, audioExtraction.args, "audio extraction invocation must match")

			whisper := findInvocation(cmds, "whisper", "--output_dir")
			require.NotNil(t, whisper, "whisper call missing")
			assert.Equal(t, []string{
				audioPath,
				"--model", "turbo",
				"--output_format", "txt",
				"--output_dir", dir,
			}, whisper.args, "whisper invocation must match")

			// Result contents: keyframes in sorted order, transcription trimmed.
			wantFrames := []string{}
			for i := 1; i <= tt.frameCount; i++ {
				wantFrames = append(wantFrames, filepath.Join(dir, fmt.Sprintf("%s_frame_%04d.jpg", stem, i)))
			}
			assert.Equal(t, wantFrames, result.keyframePaths, "keyframe paths must match created frames")
			assert.Equal(t, tt.transcript, result.transcription, "transcription must match whisper output")

			// Cleanup contract: every temp file created, in pipeline order.
			wantCleanup := append([]string{videoPath}, wantFrames...)
			wantCleanup = append(wantCleanup, audioPath)
			assert.Equal(t, wantCleanup, cleanupPaths, "cleanup paths must cover video, frames, and audio")

			// Special-character cases: args must carry the characters
			// verbatim — exec passes arguments directly (no shell), so
			// nothing may escape or mangle them.
			if tt.tempDirSuffix != "" {
				assert.Contains(t, keyframes.args[1], tt.tempDirSuffix, "input path must keep special characters intact")
				assert.Contains(t, audioExtraction.args[6], tt.tempDirSuffix, "audio output path must keep special characters intact")
				assert.Contains(t, whisper.args[6], tt.tempDirSuffix, "whisper output_dir must keep special characters intact")
			}
		})
	}
}

// TestVideoProcessVideo_ErrorPaths verifies each pipeline failure and, for
// each, the cleanup contract: every file created before the failure must be
// returned so the caller can remove it.
func TestVideoProcessVideo_ErrorPaths(t *testing.T) {
	const (
		chatID    = int64(12345)
		messageID = int64(67890)
		fileID    = "video_err"
	)

	newServer := func(t *testing.T, failDownload bool) *httptest.Server {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/file/") && !failDownload {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("fake mp4 data"))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)
		return server
	}

	t.Run("mkdir failure", func(t *testing.T) {
		// Point imageTempDir at a regular file so MkdirAll fails (ENOTDIR).
		// Unlike chmod tricks this also works when running as root.
		original := imageTempDir
		imageTempDir = filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(imageTempDir, []byte("x"), 0o644), "create blocking file")
		t.Cleanup(func() { imageTempDir = original })

		server := newServer(t, false)
		rec := &videoRecordingExec{frameCount: 1, transcript: "unreached"}
		sm := newVideoSessionManager(server.URL, rec)

		result, cleanupPaths, err := sm.processVideo(context.Background(), chatID, messageID, fileID)
		require.Error(t, err, "mkdir failure must fail")
		assert.Contains(t, err.Error(), "mkdir", "error should mention mkdir")
		assert.Nil(t, result, "no result on error")
		assert.Empty(t, cleanupPaths, "nothing created yet, no cleanup paths")
		assert.Empty(t, rec.commands(), "no external command may run")
	})

	t.Run("download failure", func(t *testing.T) {
		useVideoTempDir(t)
		server := newServer(t, true)
		rec := &videoRecordingExec{frameCount: 1, transcript: "unreached"}
		sm := newVideoSessionManager(server.URL, rec)

		result, cleanupPaths, err := sm.processVideo(context.Background(), chatID, messageID, fileID)
		require.Error(t, err, "download failure must fail")
		assert.Contains(t, err.Error(), "download video", "error should mention the download")
		assert.Nil(t, result, "no result on error")
		assert.Empty(t, cleanupPaths, "no file was written, no cleanup paths")
		assert.Empty(t, rec.commands(), "no external command may run")
	})

	t.Run("ffmpeg binary not found at keyframe stage", func(t *testing.T) {
		useVideoTempDir(t)
		server := newServer(t, false)
		rec := &videoRecordingExec{
			keyframesErr: &exec.Error{Name: "ffmpeg", Err: exec.ErrNotFound},
		}
		sm := newVideoSessionManager(server.URL, rec)

		result, cleanupPaths, err := sm.processVideo(context.Background(), chatID, messageID, fileID)
		require.Error(t, err, "missing ffmpeg must fail")
		assert.Contains(t, err.Error(), "extract keyframes", "error should identify the keyframe stage")
		assert.Contains(t, err.Error(), "ffmpeg:", "error should mention ffmpeg")
		assert.Nil(t, result, "no result on error")

		dir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
		wantCleanup := []string{filepath.Join(dir, fmt.Sprintf("%d.mp4", messageID))}
		assert.Equal(t, wantCleanup, cleanupPaths, "downloaded video must be returned for cleanup")
	})

	t.Run("ffmpeg keyframe failure keeps video for cleanup", func(t *testing.T) {
		useVideoTempDir(t)
		server := newServer(t, false)
		rec := &videoRecordingExec{
			keyframesErr: fmt.Errorf("exit status 1"),
		}
		sm := newVideoSessionManager(server.URL, rec)

		result, cleanupPaths, err := sm.processVideo(context.Background(), chatID, messageID, fileID)
		require.Error(t, err, "keyframe failure must fail")
		assert.Contains(t, err.Error(), "extract keyframes", "error should identify the keyframe stage")
		assert.Nil(t, result, "no result on error")

		dir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
		assert.Equal(t, []string{filepath.Join(dir, fmt.Sprintf("%d.mp4", messageID))}, cleanupPaths,
			"downloaded video must be returned for cleanup")
	})

	t.Run("audio extraction failure keeps video and frames", func(t *testing.T) {
		useVideoTempDir(t)
		server := newServer(t, false)
		rec := &videoRecordingExec{
			frameCount: 2,
			audioErr:   fmt.Errorf("exit status 1"),
		}
		sm := newVideoSessionManager(server.URL, rec)

		result, cleanupPaths, err := sm.processVideo(context.Background(), chatID, messageID, fileID)
		require.Error(t, err, "audio extraction failure must fail")
		assert.Contains(t, err.Error(), "extract audio", "error should identify the audio stage")
		assert.Nil(t, result, "no result on error")

		dir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
		stem := fmt.Sprintf("%d", messageID)
		wantCleanup := []string{
			filepath.Join(dir, stem+".mp4"),
			filepath.Join(dir, stem+"_frame_0001.jpg"),
			filepath.Join(dir, stem+"_frame_0002.jpg"),
		}
		assert.Equal(t, wantCleanup, cleanupPaths, "video and extracted frames must be returned for cleanup")
	})

	t.Run("whisper failure keeps video, frames, and audio", func(t *testing.T) {
		useVideoTempDir(t)
		server := newServer(t, false)
		rec := &videoRecordingExec{
			frameCount: 1,
			whisperErr: fmt.Errorf("exit status 1"),
		}
		sm := newVideoSessionManager(server.URL, rec)

		result, cleanupPaths, err := sm.processVideo(context.Background(), chatID, messageID, fileID)
		require.Error(t, err, "whisper failure must fail")
		assert.Contains(t, err.Error(), "transcribe audio", "error should identify the transcription stage")
		assert.Nil(t, result, "no result on error")

		dir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
		stem := fmt.Sprintf("%d", messageID)
		wantCleanup := []string{
			filepath.Join(dir, stem+".mp4"),
			filepath.Join(dir, stem+"_frame_0001.jpg"),
			filepath.Join(dir, stem+"_audio.wav"),
		}
		assert.Equal(t, wantCleanup, cleanupPaths, "video, frames, and extracted audio must be returned for cleanup")
	})

	t.Run("whisper output missing keeps all temp files", func(t *testing.T) {
		useVideoTempDir(t)
		server := newServer(t, false)
		rec := &videoRecordingExec{
			frameCount: 1,
			transcript: "written but not",
			skipTxt:    true,
		}
		sm := newVideoSessionManager(server.URL, rec)

		result, cleanupPaths, err := sm.processVideo(context.Background(), chatID, messageID, fileID)
		require.Error(t, err, "missing transcription must fail")
		assert.Contains(t, err.Error(), "transcribe audio", "error should identify the transcription stage")
		assert.Contains(t, err.Error(), "read transcription", "error should mention reading the transcription")
		assert.Nil(t, result, "no result on error")
		require.Len(t, cleanupPaths, 3, "video, frame, and audio must be returned for cleanup")
	})
}
