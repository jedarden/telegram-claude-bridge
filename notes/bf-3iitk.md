# Research: exec.Command Mock Patterns in telegram-claude-bridge

**Bead:** bf-3iitk  
**Date:** 2026-07-28  
**Scope:** Research exec.Command mocking patterns for subprocess testing

---

## Executive Summary

The codebase has a **well-designed mock pattern for `exec.Command`** in `internal/bridge/audio_test.go`, but it is **not fully integrated** into the production code. The pattern exists but is currently broken/incomplete.

---

## The Mock Pattern (as defined in audio_test.go)

### 1. Interface-Based Abstraction

The pattern uses two interfaces to abstract subprocess execution:

```go
// commandExec is an interface for executing commands, allowing test mocks.
type commandExec interface {
    CommandContext(ctx context.Context, name string, args ...string) command
}

// command is an interface for a running command, allowing test mocks.
type command interface {
    CombinedOutput() ([]byte, error)
    Run() error
}
```

### 2. Real Implementation (Production Wrapper)

```go
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
```

### 3. Mock Implementation (Test Double)

```go
// mockCommandExec allows test code to provide predefined command results.
type mockCommandExec struct {
    commands map[string]command // keyed by "name arg1 arg2"
}

func (m *mockCommandExec) CommandContext(ctx context.Context, name string, args ...string) command {
    key := commandKey(name, args...)
    if cmd, ok := m.commands[key]; ok {
        return cmd
    }
    return &mockCommand{err: fmt.Errorf("no mock for command: %s", key)}
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
```

### 4. Capturing Wrapper (for verification)

```go
// capturingCommandExec captures command invocations for test assertions
type capturingCommandExec struct {
    onCommand func(name string, args ...string) command
}

func (c *capturingCommandExec) CommandContext(ctx context.Context, name string, args ...string) command {
    return c.onCommand(name, args...)
}
```

---

## Current State: INCOMPLETE INTEGRATION

### The Problem

The production code in `audio.go` directly calls `exec.CommandContext`:

```go
// internal/bridge/audio.go:46
cmd := exec.CommandContext(ctx, "whisper",
    audioPath,
    "--model", "turbo",
    "--output_format", "txt",
    "--output_dir", dir,
)
```

### Compilation Error

The test tries to inject a `commandExec` field that doesn't exist:

```go
// internal/bridge/audio_test.go:342
sm := &SessionManager{
    db:          db,
    sender:      sender,
    proxyURL:    server.URL,
    commandExec: mockExec,  // ❌ This field doesn't exist!
}
```

**Error:** `unknown field commandExec in struct literal of type SessionManager`

---

## What Needs to Be Tested: processAudio Function

The `processAudio` function (lines 19-63 in audio.go):

```go
func (m *SessionManager) processAudio(
    ctx context.Context,
    chatID, messageID int64,
    content *contract.Content,
) (transcription string, cleanupPaths []string, err error) {
    // 1. Create temp directory
    dir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
    if err := os.MkdirAll(dir, 0o755); err != nil {
        return "", nil, fmt.Errorf("mkdir %s: %w", dir, err)
    }

    // 2. Determine file extension
    ext := audioFileExt(content.Type, mimeType)
    audioPath := filepath.Join(dir, stem+"."+ext)

    // 3. Download file from proxy
    if err := downloadFile(ctx, m.proxyURL+"/file/"+*content.FileID, audioPath); err != nil {
        return "", nil, fmt.Errorf("download audio: %w", err)
    }
    cleanupPaths = append(cleanupPaths, audioPath)

    // 4. Execute whisper CLI
    cmd := exec.CommandContext(ctx, "whisper", ...)  // ← NEEDS MOCKING
    out, cmdErr := cmd.CombinedOutput()
    if cmdErr != nil {
        return "", cleanupPaths, fmt.Errorf("whisper: %w\noutput: %s", cmdErr, strings.TrimSpace(string(out)))
    }

    // 5. Read transcription output
    data, err := os.ReadFile(txtPath)
    if err != nil {
        return "", cleanupPaths, fmt.Errorf("read transcription %s: %w", txtPath, err)
    }

    return strings.TrimSpace(string(data)), cleanupPaths, nil
}
```

### Test Scenarios Covered (but currently non-functional)

The test file `audio_test.go` contains comprehensive tests that would work once integrated:

1. **Argument Building Tests** (`TestProcessAudio_Args`) - Verifies correct CLI arguments
2. **Audio File Extension Tests** (`TestAudioFileExt`) - Mime type to extension mapping
3. **Whisper Output Path Tests** (`TestWhisperOutputPaths`) - File path construction
4. **Transcription Parsing Tests** (`TestTranscriptionParsing`) - Whitespace handling
5. **Error Path Tests** - Binary not found, execution failure
6. **Cleanup Tracking Tests** - Temp file cleanup verification

---

## How to Complete the Integration

### Option 1: Add Field to SessionManager (Recommended)

**Step 1:** Add the field to `SessionManager` struct:

```go
// internal/bridge/session_manager.go
type SessionManager struct {
    db             *DB
    sender         *Sender
    proxyURL       string
    workerPool     *WorkerPool
    eventPublisher events.Publishable
    ptyMgr         *PTYManager
    commandExec    commandExec  // ← ADD THIS
    // ... other fields
}
```

**Step 2:** Initialize with real implementation in production:

```go
// cmd/bridge/main.go (or wherever SessionManager is created)
sm := &SessionManager{
    db:          db,
    sender:      sender,
    proxyURL:    config.ProxyURL,
    commandExec: realCommandExec{},  // ← ADD THIS
    // ... other fields
}
```

**Step 3:** Change `audio.go` to use the field:

```go
// OLD:
cmd := exec.CommandContext(ctx, "whisper", ...)

// NEW:
cmd := m.commandExec.CommandContext(ctx, "whisper", ...)
```

**Step 4:** Same for `video.go` (uses both `ffmpeg` and `whisper`):

```go
// internal/bridge/video.go
// extractKeyframes - line 89
cmd := m.commandExec.CommandContext(ctx, "ffmpeg", ...)

// extractAudio - line 117
cmd := m.commandExec.CommandContext(ctx, "ffmpeg", ...)

// transcribeAudio - line 136
cmd := m.commandExec.CommandContext(ctx, "whisper", ...)
```

### Option 2: Package-Level Variable (Less Clean)

```go
// internal/bridge/command.go
var commandExecImpl commandExec = realCommandExec{}

// Set by tests
func SetCommandExec(ce commandExec) {
    commandExecImpl = ce
}
```

This is less clean because it requires global state management.

---

## Other Mock/Testing Patterns in the Codebase

### 1. Real Execution Helpers (updater_test.go)

**Pattern:** Execute real git commands in test helpers for acceptance testing

```go
// Helper function that executes real git commands
func runGitCommand(t *testing.T, dir string, args ...string) {
    t.Helper()

    cmd := exec.Command("git", args...)
    cmd.Dir = dir
    output, err := cmd.CombinedOutput()
    if err != nil {
        t.Fatalf("git %v failed: %v\n%s", args, err, string(output))
    }
}

// Test repository setup
func initTestRepo(t *testing.T, tempDir string) {
    runGitCommand(t, tempDir, "init")
    runGitCommand(t, tempDir, "config", "user.name", "Test User")
}
```

**Use case:** Git operations in updater tests  
**Status:** Working - uses real git in isolated test repos

### 2. Command Capture and Validation (pty_manager_test.go)

**Pattern:** Validate command structure without actual execution

```go
// Mock structure to capture intended commands
type mockExecCommand struct {
    cmd  string
    args []string
}

// Test validates command structure matches expectations
func TestTmuxCommandCapture(t *testing.T) {
    tests := []struct {
        name         string
        wantCmd      string
        wantArgs     []string
    }{
        {
            name:     "spawn pane command",
            wantCmd:  "tmux",
            wantArgs: []string{"new-window", "-t", "telegram-bridge", "-n", "t100-120"},
        },
    }

    for _, tt := range tests {
        // Validate command construction logic
        // No actual execution
    }
}
```

**Use case:** tmux command testing  
**Status:** Working - documentation-focused validation

### 3. Untested Production Code

The following files use `exec.Command` directly with **NO test coverage**:

- **`internal/bridge/video.go`** - ffmpeg for keyframe/audio extraction, whisper for transcription
- **`internal/bridge/background_jobs.go`** - shell command execution
- **`internal/bridge/pty_manager.go`** - tmux commands
- **`internal/bridge/cleanup.go`** - tmux list-panes

These would benefit from applying the interface-based pattern once it's integrated.

---

## Dependency Injection Patterns Already in Use

### Sender Mock Pattern

The `Sender` struct (proxy client) is already injected via a field:

```go
type SessionManager struct {
    sender *Sender  // ← Already using dependency injection
    // ...
}
```

Tests create mock senders:
```go
sender, err := NewSender(server.URL, filepath.Join(t.TempDir(), "sender.db"))
```

### Event Publisher Mock Pattern

Uses a `Publishable` interface with a `NullPublisher` fallback (bf-2co6 learning):

```go
type Publishable interface {
    Publish(evt Event)
}

// NullPublisher is a no-op implementation
type NullPublisher struct{}
```

---

## Other Files Using exec.Command

Based on the codebase review:

### `internal/bridge/video.go`

Uses `exec.CommandContext` for:
- `extractKeyframes()` - calls `ffmpeg` for keyframe extraction
- `extractAudio()` - calls `ffmpeg` for audio track extraction  
- `transcribeAudio()` - calls `whisper` for transcription

**Status:** No test file exists (`video_test.go` not found)

**Recommendation:** Apply the same pattern once audio_test.go is working

---

## Key Testing Helpers Already Available

The test file provides useful helper functions:

```go
// createMockWhisperSuccess creates a mock that:
// 1. Creates the expected output file
// 2. Returns success
func createMockWhisperSuccess(t *testing.T, outputDir string, stem string, transcription string) command

// createMockWhisperError creates a mock that:
// 1. Returns an error
// 2. Returns error output
func createMockWhisperError(t *testing.T, errorMsg string) command

// testAudioContext holds common test dependencies
type testAudioContext struct {
    ctx        context.Context
    tempDir    string
    chatID     int64
    messageID  int64
}
```

---

## Implementation Recommendations

### Priority 1: Fix the Compilation Error

Add the `commandExec` field to `SessionManager` and update all call sites in `audio.go` and `video.go`.

### Priority 2: Run Existing Tests

The tests in `audio_test.go` are comprehensive but currently won't compile. Once the field is added, they should pass.

### Priority 3: Create `video_test.go`

Apply the same pattern to video processing tests.

### Priority 4: Consider Extracting the Mock Infrastructure

The mock interfaces (`commandExec`, `command`) could be moved to a separate test utility file if other packages need them:

```go
// internal/testutil/command_exec_test.go
package testutil

type commandExec interface { ... }
type realCommandExec struct { ... }
type mockCommandExec struct { ... }
// etc.
```

---

## Summary

✅ **Good news:** A complete, well-designed mock pattern exists in the codebase  
❌ **Problem:** It's not integrated - the `commandExec` field doesn't exist on `SessionManager`  
🔧 **Fix:** Add the field, initialize with `realCommandExec{}` in production, use `mockCommandExec` in tests  
📋 **Work remaining:** Update `audio.go` and `video.go` to use the field instead of direct `exec.CommandContext` calls

The pattern is solid and comprehensive. It just needs the final integration step to be functional.

---

## Complete Pattern Summary

| Pattern | File(s) | Status | Use Case |
|---------|---------|--------|----------|
| **Interface injection** | `audio_test.go` | ⚠️ **Broken** (field missing) | Whisper transcription |
| **Real execution helpers** | `updater_test.go` | ✅ Working | Git operations |
| **Command validation** | `pty_manager_test.go` | ✅ Working | tmux commands |
| **No mocking** | `video.go`, `background_jobs.go`, `cleanup.go`, `pty_manager.go` | ❌ Untested | ffmpeg, shell, tmux |

---

## Key Takeaway

**The codebase lacks a consistent, working pattern for subprocess testing.** The most sophisticated pattern (interface-based injection in `audio_test.go`) is **not integrated** into the production code, and most external command execution has **no test coverage at all**.

To make the tests functional:
1. Add `commandExec commandExec` field to `SessionManager` struct
2. Initialize with `realCommandExec{}` in production
3. Update `audio.go` and `video.go` to use `m.commandExec.CommandContext()` instead of `exec.CommandContext()`
4. Tests will then be able to inject `mockCommandExec` for verification
