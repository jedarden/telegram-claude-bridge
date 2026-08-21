package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── tmux Mock Fixtures ─────────────────────────────────────────────────────────────

const tmuxMockFixtureDirEnv = "TELEGRAM_BRIDGE_TMUX_FIXTURE_DIR"

// tmuxMockResponse describes the result returned by the fixture-backed tmux
// executable for one subcommand.
type tmuxMockResponse struct {
	stdout   string
	stderr   string
	exitCode int
}

const (
	tmuxNewSessionOutput   = ""
	tmuxNewPaneOutput      = "%1\n"
	tmuxListPanesOutput    = "%1\ttelegram-bridge\tt100-10\t0\t12345\t/dev/pts/42\n"
	tmuxListWindowsOutput  = "t100-10\nw-worker_1-1234567890\n"
	tmuxPaneTTYOutput      = "/dev/pts/42\n"
	tmuxCapturePaneOutput  = "● Mock response\nMock response body\n❯\n"
	tmuxCaptureShellOutput = "shell command output\n"
)

// tmuxCommandFixtures returns independent fixtures for the tmux commands used
// by PTYManager and cleanup code. A function is used instead of a package-level
// mutable map so tests can safely override one command without affecting other
// tests.
func tmuxCommandFixtures() map[string]tmuxMockResponse {
	return map[string]tmuxMockResponse{
		"has-session":     {exitCode: 0},
		"new-session":     {stdout: tmuxNewSessionOutput},
		"new-window":      {stdout: tmuxNewSessionOutput},
		"new-pane":        {stdout: tmuxNewPaneOutput},
		"list-panes":      {stdout: tmuxListPanesOutput},
		"list-windows":    {stdout: tmuxListWindowsOutput},
		"display-message": {stdout: tmuxPaneTTYOutput},
		"capture-pane":    {stdout: tmuxCapturePaneOutput},
		// capture-shell is kept as a separate fixture because callers that
		// execute tmux through a shell use this response name, while the
		// PTYManager's screen capture path uses capture-pane.
		"capture-shell": {stdout: tmuxCaptureShellOutput},
		"kill-window":   {stdout: tmuxNewSessionOutput},
		"send-keys":     {stdout: tmuxNewSessionOutput},
		"set-buffer":    {stdout: tmuxNewSessionOutput},
		"paste-buffer":  {stdout: tmuxNewSessionOutput},
	}
}

// tmuxTestState owns the temporary fixture directory and fake tmux executable
// installed by setupTmuxTest.
type tmuxTestState struct {
	fixtureDir         string
	shellExec          func(args ...string) ([]byte, error)
	previousPath       string
	previousPathSet    bool
	previousFixtureDir string
	previousFixtureSet bool
	tornDown           bool
}

// setupTmuxTest installs a fixture-backed tmux command and loads the common
// command responses. The returned state can be used to inspect recorded calls
// or to explicitly invoke teardownTmuxTest; t.Cleanup also tears it down.
func setupTmuxTest(t *testing.T) *tmuxTestState {
	t.Helper()

	fixtureDir := t.TempDir()
	tmuxPath := filepath.Join(fixtureDir, "tmux")
	const tmuxMockScript = `#!/bin/sh
set -eu

fixture_dir=${TELEGRAM_BRIDGE_TMUX_FIXTURE_DIR:?missing fixture directory}
if [ "$#" -eq 0 ]; then
    echo "tmux mock: missing subcommand" >&2
    exit 127
fi

command=$1
printf '%s\n' "$*" >> "$fixture_dir/calls"

stdout_file="$fixture_dir/$command.stdout"
stderr_file="$fixture_dir/$command.stderr"
exit_file="$fixture_dir/$command.exit"
if [ ! -f "$stdout_file" ] || [ ! -f "$stderr_file" ] || [ ! -f "$exit_file" ]; then
    echo "tmux mock: no fixture for $command" >&2
    exit 127
fi

cat "$stdout_file"
cat "$stderr_file" >&2
exit "$(cat "$exit_file")"
`
	if err := os.WriteFile(tmuxPath, []byte(tmuxMockScript), 0o755); err != nil {
		t.Fatalf("write tmux mock: %v", err)
	}

	previousFixtureDir, previousFixtureSet := os.LookupEnv(tmuxMockFixtureDirEnv)
	previousPath, previousPathSet := os.LookupEnv("PATH")
	t.Setenv(tmuxMockFixtureDirEnv, fixtureDir)
	t.Setenv("PATH", fixtureDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	state := &tmuxTestState{
		fixtureDir:         fixtureDir,
		previousPath:       previousPath,
		previousPathSet:    previousPathSet,
		previousFixtureDir: previousFixtureDir,
		previousFixtureSet: previousFixtureSet,
	}
	state.shellExec = func(args ...string) ([]byte, error) {
		return mockShellExec(state.fixtureDir, args...)
	}
	for command, response := range tmuxCommandFixtures() {
		writeTmuxMockResponse(t, fixtureDir, command, response)
	}
	t.Cleanup(func() { teardownTmuxTest(t, state) })
	return state
}

// teardownTmuxTest removes the temporary fixture state. It is idempotent so a
// test may clean up early while the registered t.Cleanup remains in place.
func teardownTmuxTest(t *testing.T, state *tmuxTestState) {
	t.Helper()
	if state == nil || state.tornDown {
		return
	}
	state.tornDown = true
	var err error
	if state.previousPathSet {
		err = os.Setenv("PATH", state.previousPath)
	} else {
		err = os.Unsetenv("PATH")
	}
	if err != nil {
		t.Errorf("restore PATH after tmux test: %v", err)
	}
	if state.previousFixtureSet {
		err = os.Setenv(tmuxMockFixtureDirEnv, state.previousFixtureDir)
	} else {
		err = os.Unsetenv(tmuxMockFixtureDirEnv)
	}
	if err != nil {
		t.Errorf("restore %s after tmux test: %v", tmuxMockFixtureDirEnv, err)
	}
	// Worker goroutines leaked by spawn_worker tests may still be writing to
	// the fixture directory (the mock appends every invocation to its calls
	// log) when this cleanup runs; retry briefly until they drain instead of
	// racing them and failing with "directory not empty".
	for attempt := 0; ; attempt++ {
		err = os.RemoveAll(state.fixtureDir)
		if err == nil || attempt >= 9 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Errorf("remove tmux fixture directory: %v", err)
	}
}

// mockTmuxCommand writes a complete response fixture for one tmux subcommand.
func mockTmuxCommand(t *testing.T, command, output string) {
	t.Helper()
	mockTmuxResponse(t, command, tmuxMockResponse{stdout: output})
}

// mockTmuxCommandResult writes a response fixture with an explicit exit code.
func mockTmuxCommandResult(t *testing.T, command, output string, exitCode int) {
	t.Helper()
	mockTmuxResponse(t, command, tmuxMockResponse{stdout: output, exitCode: exitCode})
}

// mockTmuxCommandFailure writes a failed tmux response with stderr output.
func mockTmuxCommandFailure(t *testing.T, command, stderr string, exitCode int) {
	t.Helper()
	mockTmuxResponse(t, command, tmuxMockResponse{stderr: stderr, exitCode: exitCode})
}

func mockTmuxResponse(t *testing.T, command string, response tmuxMockResponse) {
	t.Helper()
	if command == "" || strings.ContainsAny(command, "/\n") {
		t.Fatalf("invalid tmux mock command %q", command)
	}
	fixtureDir := os.Getenv(tmuxMockFixtureDirEnv)
	if fixtureDir == "" {
		t.Fatal("setupTmuxTest must be called before mocking a tmux command")
	}
	writeTmuxMockResponse(t, fixtureDir, command, response)
}

func writeTmuxMockResponse(t *testing.T, fixtureDir, command string, response tmuxMockResponse) {
	t.Helper()
	files := map[string][]byte{
		filepath.Join(fixtureDir, command+".stdout"): []byte(response.stdout),
		filepath.Join(fixtureDir, command+".stderr"): []byte(response.stderr),
		filepath.Join(fixtureDir, command+".exit"):   []byte(strconv.Itoa(response.exitCode)),
	}
	for path, contents := range files {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatalf("write tmux fixture %q: %v", path, err)
		}
	}
}

// mockShellExec returns the predefined response for a tmux command. It reads
// the same fixture files as the fake tmux executable installed by
// setupTmuxTest, so tests can exercise the mock directly and production code
// can exercise it through exec.Command without maintaining two response
// implementations.
func mockShellExec(fixtureDir string, args ...string) ([]byte, error) {
	if fixtureDir == "" {
		return nil, fmt.Errorf("tmux shell mock: missing fixture directory")
	}
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("tmux shell mock: missing subcommand")
	}

	command := args[0]
	stdout, err := os.ReadFile(filepath.Join(fixtureDir, command+".stdout"))
	if err != nil {
		return nil, fmt.Errorf("tmux shell mock: read %s stdout: %w", command, err)
	}
	stderr, err := os.ReadFile(filepath.Join(fixtureDir, command+".stderr"))
	if err != nil {
		return nil, fmt.Errorf("tmux shell mock: read %s stderr: %w", command, err)
	}
	exitBytes, err := os.ReadFile(filepath.Join(fixtureDir, command+".exit"))
	if err != nil {
		return nil, fmt.Errorf("tmux shell mock: read %s exit code: %w", command, err)
	}
	exitCode, err := strconv.Atoi(strings.TrimSpace(string(exitBytes)))
	if err != nil {
		return nil, fmt.Errorf("tmux shell mock: parse %s exit code: %w", command, err)
	}

	output := make([]byte, 0, len(stdout)+len(stderr))
	output = append(output, stdout...)
	output = append(output, stderr...)
	if exitCode != 0 {
		return output, fmt.Errorf("tmux shell mock: %s exited with status %d", command, exitCode)
	}
	return output, nil
}

// tmuxCalls returns the command lines sent to the fixture-backed tmux binary.
func (s *tmuxTestState) tmuxCalls(t *testing.T) []string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(s.fixtureDir, "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read tmux calls: %v", err)
	}
	contents = []byte(strings.TrimSpace(string(contents)))
	if len(contents) == 0 {
		return nil
	}
	return strings.Split(string(contents), "\n")
}

// ── Helper Function Tests ─────────────────────────────────────────────────────────

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "'simple'"},
		{"with space", "'with space'"},
		{"with'apostrophe", "'with'\\''apostrophe'"},
		{"with'two'apostrophes", "'with'\\''two'\\''apostrophes'"},
		{"path/to/file", "'path/to/file'"},
		{"", "''"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := shellQuote(tc.input)
			if got != tc.expected {
				t.Errorf("shellQuote(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestBridgeRespFile(t *testing.T) {
	tests := []struct {
		paneName string
	}{
		{"t100-10"},
		{"simple"},
		{"pane-with-dashes"},
	}

	for _, tc := range tests {
		t.Run(tc.paneName, func(t *testing.T) {
			got := bridgeRespFile(tc.paneName)
			want := filepath.Join(os.TempDir(), "telegram-bridge-resp", tc.paneName+".resp")
			if got != want {
				t.Errorf("bridgeRespFile(%q) = %q, want %q", tc.paneName, got, want)
			}
		})
	}
}

// TestPaneNaming_FormatPaneTarget covers the pane target formatting performed
// by SpawnPane. Pane names are intentionally passed through unchanged: tmux
// window names can contain characters beyond the names generated by the
// bridge, so formatting must only add the session separator.
func TestPaneNaming_FormatPaneTarget(t *testing.T) {
	tests := []struct {
		name         string
		paneName     string
		wantTarget   string
		newWindowErr string
		wantSpawnErr bool
	}{
		{
			name:       "session pane",
			paneName:   "t1003602927203-120",
			wantTarget: "telegram-bridge:t1003602927203-120",
		},
		{
			name:       "general topic pane",
			paneName:   "t1003602927203-0",
			wantTarget: "telegram-bridge:t1003602927203-0",
		},
		{
			name:       "worker pane",
			paneName:   "w-worker_1-1700000000",
			wantTarget: "telegram-bridge:w-worker_1-1700000000",
		},
		{
			name:       "empty pane name",
			paneName:   "",
			wantTarget: "telegram-bridge:",
		},
		{
			name:       "spaces and punctuation",
			paneName:   "pane with spaces!",
			wantTarget: "telegram-bridge:pane with spaces!",
		},
		{
			name:       "additional tmux separator",
			paneName:   "group:child",
			wantTarget: "telegram-bridge:group:child",
		},
		{
			name:         "new window failure",
			paneName:     "t123-4",
			newWindowErr: "new window failed",
			wantSpawnErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupTmuxTest(t)
			if tc.newWindowErr != "" {
				mockTmuxCommandFailure(t, "new-window", tc.newWindowErr, 1)
			}

			got, err := NewPTYManager().SpawnPane(tc.paneName, t.TempDir(), nil)
			if tc.wantSpawnErr {
				if err == nil {
					t.Fatalf("SpawnPane(%q) error = nil, want an error", tc.paneName)
				}
				if !strings.Contains(err.Error(), tc.newWindowErr) {
					t.Errorf("SpawnPane(%q) error = %q, want it to contain %q", tc.paneName, err, tc.newWindowErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SpawnPane(%q) returned error: %v", tc.paneName, err)
			}
			if got != tc.wantTarget {
				t.Errorf("SpawnPane(%q) target = %q, want %q", tc.paneName, got, tc.wantTarget)
			}
		})
	}
}

// TestPaneNaming_ParsePaneIdentifier covers the pane-name extraction used by
// WaitForResponse. The response is written to the expected stop-hook path so
// the test observes which part of each target the production code selected.
func TestPaneNaming_ParsePaneIdentifier(t *testing.T) {
	tests := []struct {
		name         string
		paneTarget   string
		wantPaneName string
	}{
		{
			name:         "standard target",
			paneTarget:   "telegram-bridge:t100-10",
			wantPaneName: "t100-10",
		},
		{
			name:         "target without session",
			paneTarget:   "t100-10",
			wantPaneName: "t100-10",
		},
		{
			name:         "last colon separates target",
			paneTarget:   "telegram:bridge:t100-10",
			wantPaneName: "t100-10",
		},
		{
			name:         "empty pane name",
			paneTarget:   "telegram-bridge:",
			wantPaneName: "",
		},
		{
			name:         "empty target",
			paneTarget:   "",
			wantPaneName: "",
		},
		{
			name:         "leading colon",
			paneTarget:   ":t100-10",
			wantPaneName: "t100-10",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupTmuxTest(t)
			respFile := bridgeRespFile(tc.wantPaneName)
			readyFile := respFile + ".ready"
			t.Cleanup(func() {
				os.Remove(respFile)
				os.Remove(readyFile)
			})

			writeErr := make(chan error, 1)
			go func() {
				time.Sleep(25 * time.Millisecond)
				if err := os.WriteFile(respFile, []byte("response for "+tc.wantPaneName), 0o600); err != nil {
					writeErr <- err
					return
				}
				writeErr <- os.WriteFile(readyFile, []byte("ready"), 0o600)
			}()

			got, err := NewPTYManager().WaitForResponse(context.Background(), tc.paneTarget, "", nil)
			if writeErr := <-writeErr; writeErr != nil {
				t.Fatalf("write stop-hook fixture: %v", writeErr)
			}
			if err != nil {
				t.Fatalf("WaitForResponse(%q) returned error: %v", tc.paneTarget, err)
			}
			want := "response for " + tc.wantPaneName
			if got != want {
				t.Errorf("WaitForResponse(%q) = %q, want %q", tc.paneTarget, got, want)
			}
		})
	}
}

func TestWaitForResponseWithSource(t *testing.T) {
	t.Run("stop hook", func(t *testing.T) {
		setupTmuxTest(t)
		paneTarget := "telegram-bridge:source-stop-hook"
		respFile := bridgeRespFile("source-stop-hook")
		t.Cleanup(func() {
			os.Remove(respFile)
			os.Remove(respFile + ".ready")
		})

		go func() {
			time.Sleep(25 * time.Millisecond)
			_ = os.WriteFile(respFile, []byte("hook response"), 0o600)
			_ = os.WriteFile(respFile+".ready", []byte("ready"), 0o600)
		}()

		got, source, err := NewPTYManager().WaitForResponseWithSource(context.Background(), paneTarget, "", nil)
		if err != nil {
			t.Fatalf("WaitForResponseWithSource() returned error: %v", err)
		}
		if got != "hook response" {
			t.Errorf("response = %q, want %q", got, "hook response")
		}
		if source != ResponseSourceStopHook {
			t.Errorf("source = %q, want %q", source, ResponseSourceStopHook)
		}
	})

	t.Run("pty fallback", func(t *testing.T) {
		setupTmuxTest(t)
		paneTarget := "telegram-bridge:source-pty-fallback"
		respFile := bridgeRespFile("source-pty-fallback")
		t.Cleanup(func() {
			os.Remove(respFile)
			os.Remove(respFile + ".ready")
		})

		got, source, err := NewPTYManager().WaitForResponseWithSource(context.Background(), paneTarget, "", nil)
		if err != nil {
			t.Fatalf("WaitForResponseWithSource() returned error: %v", err)
		}
		if got != "Mock response\nMock response body" {
			t.Errorf("response = %q, want PTY fixture response", got)
		}
		if source != ResponseSourcePTY {
			t.Errorf("source = %q, want %q", source, ResponseSourcePTY)
		}
	})
}

func TestWaitForStartupContextCancellation(t *testing.T) {
	setupTmuxTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := NewPTYManager().WaitForStartupContext(ctx, "telegram-bridge:startup-cancel")
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("WaitForStartupContext() error = %v, want context canceled", err)
	}
}

func TestIsUIChrome(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"──────────────────", true},
		{"══════════════════", true},
		{"━━━━━━━━━━━━━━━━", true},
		{"───────────────", true},
		{"────────text────", false}, // Has text mixed in
		{"text", false},
		{"", false},
		{"─", true},
		{"=", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := isUIChrome(tc.input)
			if got != tc.expected {
				t.Errorf("isUIChrome(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestIsTimingLine(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"✻ Brewed for 5s", true},
		{"✢ Contemplating…", true},
		{"✽ Cooking… (5s · ↑ 1000 tokens)", true},
		{"* Word… (2s · …)", true},
		{"* Working... (10s)", true},
		{"* Process started", false}, // No ellipsis or parens after the verb
		{"✻ Reading files", true},
		{"✻ Done", true},
		{"✻", true},
		{"✢", true},
		{"✽", true},
		{"* not a timing line", false},
		{"*no space", false},
		{"regular text", false},
		{"", false},
		{"*", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := isTimingLine(tc.input)
			if got != tc.expected {
				t.Errorf("isTimingLine(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestIsActiveProgressLine(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"Reading 1 file…", true},
		{"Reading 2 file…", true},
		{"Reading 100 files…", true},
		{"Reading 1 file… (ctrl+o to expand)", true},
		{"Reading", false},
		{"Reading something else", false},
		{"reading 1 file…", false}, // Case sensitive
		{"", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := isActiveProgressLine(tc.input)
			if got != tc.expected {
				t.Errorf("isActiveProgressLine(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestIsToolCallLine(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		// Valid tool calls
		{"Bash(cmd)", true},
		{"Read(path)", true},
		{"Write(file)", true},
		{"Edit(path)", true},
		{"WebSearch(query)", true},
		{"LSP(operation)", true},
		{"Tool123_name(arg)", true},
		{"MyTool(arg)", true},

		// With spinner prefix (non-ASCII)
		{"⠋ Bash(cmd)", true},
		{"⠁⠁⠁ Read(file)", true},
		{"⣶ Edit(path)", true},

		// Invalid - not starting with capital letter
		{"bash(cmd)", false},
		{"read(path)", false},
		{"123tool(arg)", false},

		// Invalid - no parentheses
		{"Bash", false},
		{"Read", false},
		{"Bash cmd", false},

		// Empty parentheses still match the Tool(args) shape
		{"Bash()", true},

		// Invalid - special chars in name
		{"Bash-Test(cmd)", false},
		{"Tool.Name(arg)", false},

		// Edge cases
		{"", false},
		{"()", false},
		{"Bash)(", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := isToolCallLine(tc.input)
			if got != tc.expected {
				t.Errorf("isToolCallLine(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestResponseComplete(t *testing.T) {
	tests := []struct {
		name     string
		screen   string
		expected bool
	}{
		{
			name:     "complete with prompt after bullet",
			screen:   "some text\n● Response here\n❯",
			expected: true,
		},
		{
			name:     "complete with extra spacing",
			screen:   "text\n● Response\n  \n❯",
			expected: true,
		},
		{
			name:     "no bullet present",
			screen:   "some text\n❯",
			expected: false,
		},
		{
			name:     "no prompt after bullet",
			screen:   "text\n● Response\nmore text",
			expected: false,
		},
		{
			name:     "active timing line blocks completion",
			screen:   "● Response\n✻ Brewed for 2s\n❯",
			expected: false,
		},
		{
			name:     "active cooking line blocks completion",
			screen:   "● Response\n✽ Cooking…\n❯",
			expected: false,
		},
		{
			name:     "reading progress blocks completion",
			screen:   "● Response\nReading 2 files…\n❯",
			expected: false,
		},
		{
			name:     "prompt appears before bullet",
			screen:   "❯\ntext\n● Response",
			expected: false,
		},
		{
			name:     "multiple bullets, last one has prompt",
			screen:   "● First\n● Second\n❯",
			expected: true,
		},
		{
			name:     "empty screen",
			screen:   "",
			expected: false,
		},
		{
			name:     "only bullet no prompt",
			screen:   "● Response\nmore",
			expected: false,
		},
		{
			name:     "only prompt no bullet",
			screen:   "❯",
			expected: false,
		},
		{
			name:     "bullet then timing lines then prompt",
			screen:   "● Response\n✻ Brewed for 2s\n✢ Contemplating…\n❯",
			expected: false,
		},
		{
			name:     "reading files after prompt",
			screen:   "● Response\n❯\nReading 5 files…",
			expected: false,
		},
		{
			name:     "cooking after prompt",
			screen:   "● Response\n❯\n✽ Cooking… (10s · ↑ 1000 tokens)",
			expected: false,
		},
		{
			name:     "multiple active indicators after prompt",
			screen:   "● Response\n❯\n✻ Done\nReading 2 files…",
			expected: false,
		},
		{
			name:     "spaces in active lines",
			screen:   "● Response\n  Reading 2 files…  \n❯",
			expected: false,
		},
		{
			name:     "completion with various timing lines before",
			screen:   "● Response\n✻ Done\n✢ Contemplating…\n✽ Cooking…\n❯",
			expected: false, // "✽ Cooking…" is an active timing indicator after bullet
		},
		{
			name:     "asterisk form timing line blocks",
			screen:   "● Response\n* Brewed for 5s (2s · ↑ 100 tokens)\n❯",
			expected: false,
		},
		{
			name:     "multiple prompts after last bullet",
			screen:   "● Response\n❯ some text\n❯",
			expected: true,
		},
		{
			name:     "prompt in middle of response",
			screen:   "● Response\n❯ some text\nmore response",
			expected: true, // prompt after bullet means complete, even with text after
		},
		{
			name:     "old prompt before new bullet",
			screen:   "❯\nold prompt\n● New response\n❯",
			expected: true,
		},
		{
			name:     "complex screen with multiple bullets",
			screen:   "● First response\n❯\n● Second response\n❯\n● Third\n❯",
			expected: true,
		},
		{
			name:     "unicode bullet variant",
			screen:   "● Response\ndone\n❯",
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := responseComplete(tc.screen)
			if got != tc.expected {
				t.Errorf("responseComplete() = %v, want %v\nscreen was: %q", got, tc.expected, tc.screen)
			}
		})
	}
}

func TestResponseComplete_EdgeCases(t *testing.T) {
	t.Run("reading with ctrl+o text", func(t *testing.T) {
		screen := "● Response\nReading 1 file… (ctrl+o to expand)\n❯"
		if responseComplete(screen) {
			t.Error("expected false when reading progress is visible")
		}
	})

	t.Run("reading files plural", func(t *testing.T) {
		screen := "● Response\nReading 100 files…\n❯"
		if responseComplete(screen) {
			t.Error("expected false when reading multiple files")
		}
	})

	t.Run("reading file not ellipsis", func(t *testing.T) {
		screen := "● Response\nReading 1 file\n❯"
		if responseComplete(screen) {
			t.Error("expected false for reading without ellipsis")
		}
	})

	t.Run("empty lines between bullet and prompt", func(t *testing.T) {
		screen := "● Response\n\n\n\n❯"
		if !responseComplete(screen) {
			t.Error("expected true with empty lines between bullet and prompt")
		}
	})

	t.Run("very long response", func(t *testing.T) {
		lines := make([]string, 1000)
		lines[0] = "● Response start"
		for i := 1; i < 999; i++ {
			lines[i] = "Line " + strconv.Itoa(i)
		}
		lines[999] = "❯"
		screen := strings.Join(lines, "\n")
		if !responseComplete(screen) {
			t.Error("expected true for very long response")
		}
	})

	t.Run("timing line mixed with response", func(t *testing.T) {
		screen := "● Response\nSome text\n✻ Brewed for 1s\nMore text\n❯"
		if responseComplete(screen) {
			t.Error("expected false when timing line appears after bullet")
		}
	})

	t.Run("active indicator after timing line", func(t *testing.T) {
		screen := "● Response\n✻ Brewed for 1s\nReading 2 files…\n❯"
		if responseComplete(screen) {
			t.Error("expected false when active progress follows timing")
		}
	})
}

func TestExtractResponseText(t *testing.T) {
	tests := []struct {
		name     string
		screen   string
		expected string
	}{
		{
			name:     "simple response after bullet",
			screen:   "pre\n● Response text\n❯",
			expected: "Response text",
		},
		{
			name:     "multiline response",
			screen:   "pre\n● Line 1\nLine 2\nLine 3\n❯",
			expected: "Line 1\nLine 2\nLine 3",
		},
		{
			name:     "inline text on bullet line",
			screen:   "pre\n● Response here\nmore\n❯",
			expected: "Response here\nmore",
		},
		{
			name:     "filters timing lines",
			screen:   "pre\n● Text\n✻ Brewed for 2s\nmore\n❯",
			expected: "Text\nmore",
		},
		{
			name:     "filters tool calls",
			screen:   "pre\n● Response\nBash(do something)\nResult\n❯",
			expected: "Response\nResult",
		},
		{
			name:     "filters tool output",
			screen:   "pre\n● Text\n⎿ tool output\nmore\n❯",
			expected: "Text\nmore",
		},
		{
			name:     "filters UI chrome",
			screen:   "pre\n● Text\n──────────\nmore\n❯",
			expected: "Text\nmore",
		},
		{
			name:     "filters asterisk timing",
			screen:   "pre\n● Text\n* Word… (2s)\nmore\n❯",
			expected: "Text\nmore",
		},
		{
			name:     "no bullet",
			screen:   "some text\n❯",
			expected: "",
		},
		{
			name:     "bullet with no prompt",
			screen:   "● Text\nmore",
			expected: "Text\nmore",
		},
		{
			name:     "empty screen",
			screen:   "",
			expected: "",
		},
		{
			name:     "preserves empty lines",
			screen:   "pre\n● Line 1\n\nLine 3\n❯",
			expected: "Line 1\n\nLine 3",
		},
		{
			name:     "trims whitespace",
			screen:   "pre\n● Text  \n  more  \n❯",
			expected: "Text\n  more", // only outer whitespace is trimmed; internal indentation survives
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractResponseText(tc.screen)
			if got != tc.expected {
				t.Errorf("extractResponseText() =\n%q\nwant:\n%q", got, tc.expected)
			}
		})
	}
}

func TestExtractResponseText_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		screen   string
		expected string
	}{
		{
			name:     "bullet at start of line with text",
			screen:   "●Response immediately\n❯",
			expected: "Response immediately",
		},
		{
			name:     "multiple bullets uses last",
			screen:   "● First\n● Second\n❯",
			expected: "Second",
		},
		{
			name:     "empty bullet line",
			screen:   "●\n\n❯",
			expected: "",
		},
		{
			name:     "bullet with only spaces after",
			screen:   "●    \n❯",
			expected: "",
		},
		{
			name:     "tool call with spinner",
			screen:   "● Text\n⠋ Bash(cmd)\n⎿ output\n❯",
			expected: "Text",
		},
		{
			name:     "multiple timing variants",
			screen:   "● Text\n✻ Brewed\n✢ Contemplating\n✽ Cooking\nmore\n❯",
			expected: "Text\nmore",
		},
		{
			name:     "all timing lines",
			screen:   "● Text\n✻ Done\n✢ Thinking\n✽ Cooking\n❯",
			expected: "Text",
		},
		{
			name:     "tool call with underscores",
			screen:   "● Text\nTool_Name_123(arg)\nresult\n❯",
			expected: "Text\nresult",
		},
		{
			name:     "nested tool calls",
			screen:   "● Text\nBash(cmd1)\nBash(cmd2)\nBash(cmd3)\n❯",
			expected: "Text",
		},
		{
			name:     "reading progress line",
			screen:   "● Text\nReading 5 files… (ctrl+o to expand)\n❯",
			expected: "Text",
		},
		{
			name:     "mixed UI chrome styles",
			screen:   "● Text\n──────────\n══════════\n━━━━━━━━\nmore\n❯",
			expected: "Text\nmore",
		},
		{
			name:     "unicode in response",
			screen:   "● Hello 世界 🌍\nMore text\n❯",
			expected: "Hello 世界 🌍\nMore text",
		},
		{
			name:     "tool output with unicode",
			screen:   "● Text\n⎿ Unicode output: 你好\nresult\n❯",
			expected: "Text\nresult",
		},
		{
			name:     "very long single line",
			screen:   "● " + strings.Repeat("A", 10000) + "\n❯",
			expected: strings.Repeat("A", 10000),
		},
		{
			name:     "many lines",
			screen:   "● start\n" + strings.Join(make([]string, 100), "\n") + "\n❯",
			expected: "start", // Empty lines are preserved but result is trimmed
		},
		{
			name:     "preserves internal whitespace",
			screen:   "● Text\n  indented\n\ttabbed\n❯",
			expected: "Text\n  indented\n\ttabbed",
		},
		{
			name:     "prompt between bullets uses last bullet",
			screen:   "● First\n❯\n● Second\n❯",
			expected: "Second", // extraction anchors on the last ●, ignoring earlier prompts
		},
		{
			name:     "no prompt extracts everything after bullet",
			screen:   "● First\nSecond\nThird",
			expected: "First\nSecond\nThird",
		},
		{
			name:     "bullet with special characters",
			screen:   "● Response with $pecial & characters!\n❯",
			expected: "Response with $pecial & characters!",
		},
		{
			name:     "asterisk form with ellipsis",
			screen:   "● Text\n* Process started… (5s)\nmore\n❯",
			expected: "Text\nmore",
		},
		{
			name:     "asterisk form with dots",
			screen:   "● Text\n* Working... (10s)\nmore\n❯",
			expected: "Text\nmore",
		},
		{
			name:     "reading files not ellipsis is kept",
			screen:   "● Text\nReading file\n❯",
			expected: "Text", // "Reading file" matches active progress pattern and is filtered
		},
		{
			name:     "reading without ellipsis not filtered",
			screen:   "● Text\nReading something\n❯",
			expected: "Text\nReading something", // "Reading something" doesn't contain "file" so it's kept
		},
		{
			name:     "old prompt before bullet ignored",
			screen:   "❯ old\n● New response\n❯",
			expected: "New response",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractResponseText(tc.screen)
			if got != tc.expected {
				t.Errorf("extractResponseText() =\n%q\nwant:\n%q", got, tc.expected)
			}
		})
	}
}

func TestExtractResponseText_ActiveProgressDetection(t *testing.T) {
	tests := []struct {
		name     string
		screen   string
		expected string
	}{
		{
			name:     "reading 1 file",
			screen:   "● Text\nReading 1 file…\n❯",
			expected: "Text",
		},
		{
			name:     "reading multiple files",
			screen:   "● Text\nReading 123 files…\n❯",
			expected: "Text",
		},
		{
			name:     "reading with expand hint",
			screen:   "● Text\nReading 5 files… (ctrl+o to expand)\n❯",
			expected: "Text",
		},
		{
			name:     "reading without ellipsis filtered",
			screen:   "● Text\nReading 5 files\n❯",
			expected: "Text", // the ellipsis is not part of the match — prefix + "file" is enough
		},
		{
			name:     "reading lowercase not kept",
			screen:   "● Text\nreading 1 file…\n❯",
			expected: "Text\nreading 1 file…",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractResponseText(tc.screen)
			if got != tc.expected {
				t.Errorf("extractResponseText() =\n%q\nwant:\n%q", got, tc.expected)
			}
		})
	}
}

func TestExtractResponseText_ToolCallDetection(t *testing.T) {
	tests := []struct {
		name     string
		screen   string
		expected string
	}{
		{
			name:     "basic tool call",
			screen:   "● Text\nBash(cmd)\n❯",
			expected: "Text",
		},
		{
			name:     "tool with arguments",
			screen:   "● Text\nRead(/path/to/file)\n❯",
			expected: "Text",
		},
		{
			name:     "tool with complex args",
			screen:   "● Text\nWebSearch(query=\"test\")\n❯",
			expected: "Text",
		},
		{
			name:     "tool with underscores",
			screen:   "● Text\nTool_Name_123(arg)\n❯",
			expected: "Text",
		},
		{
			name:     "braille spinner prefix",
			screen:   "● Text\n⠋ Bash(cmd)\n❯",
			expected: "Text",
		},
		{
			name:     "multiple braille spinners",
			screen:   "● Text\n⠁⠁⠁ Read(file)\n❯",
			expected: "Text",
		},
		{
			name:     "lowercase tool not filtered",
			screen:   "● Text\nbash(cmd)\n❯",
			expected: "Text\nbash(cmd)",
		},
		{
			name:     "tool without parentheses not filtered",
			screen:   "● Text\nBash cmd\n❯",
			expected: "Text\nBash cmd",
		},
		{
			name:     "empty parentheses filtered",
			screen:   "● Text\nBash()\n❯",
			expected: "Text", // "Bash()" still matches the tool-call shape
		},
		{
			name:     "tool with hyphen not filtered",
			screen:   "● Text\nBash-Test(cmd)\n❯",
			expected: "Text\nBash-Test(cmd)",
		},
		{
			name:     "tool with dot not filtered",
			screen:   "● Text\nTool.Name(cmd)\n❯",
			expected: "Text\nTool.Name(cmd)",
		},
		{
			name:     "number start not filtered",
			screen:   "● Text\n123Tool(cmd)\n❯",
			expected: "Text\n123Tool(cmd)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractResponseText(tc.screen)
			if got != tc.expected {
				t.Errorf("extractResponseText() =\n%q\nwant:\n%q", got, tc.expected)
			}
		})
	}
}

func TestReadStopHookResponse(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name         string
		setupReady   bool
		setupResp    bool
		setupContent string
		expectedText string
		expectedOK   bool
	}{
		{
			name:         "ready file exists with content",
			setupReady:   true,
			setupResp:    true,
			setupContent: "response text",
			expectedText: "response text",
			expectedOK:   true,
		},
		{
			name:         "ready file exists with multiline",
			setupReady:   true,
			setupResp:    true,
			setupContent: "line 1\nline 2\nline 3",
			expectedText: "line 1\nline 2\nline 3",
			expectedOK:   true,
		},
		{
			name:         "ready file exists empty",
			setupReady:   true,
			setupResp:    true,
			setupContent: "",
			expectedText: "",
			expectedOK:   true,
		},
		{
			name:         "ready file missing",
			setupReady:   false,
			setupResp:    false,
			expectedText: "",
			expectedOK:   false,
		},
		{
			name:         "ready file exists but resp file missing",
			setupReady:   true,
			setupResp:    false,
			expectedText: "",
			expectedOK:   false,
		},
		{
			name:         "trailing newlines are trimmed",
			setupReady:   true,
			setupResp:    true,
			setupContent: "text\n\n\n",
			expectedText: "text",
			expectedOK:   true,
		},
		{
			name:         "content with Windows line endings",
			setupReady:   true,
			setupResp:    true,
			setupContent: "line 1\r\nline 2\r\n",
			expectedText: "line 1\r\nline 2\r",
			expectedOK:   true,
		},
		{
			name:         "only newlines",
			setupReady:   true,
			setupResp:    true,
			setupContent: "\n\n\n\n",
			expectedText: "",
			expectedOK:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			respFile := filepath.Join(tempDir, "test.resp")
			readyFile := respFile + ".ready"

			if tc.setupReady {
				if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
					t.Fatalf("write ready file: %v", err)
				}
			}
			if tc.setupResp {
				if err := os.WriteFile(respFile, []byte(tc.setupContent), 0600); err != nil {
					t.Fatalf("write resp file: %v", err)
				}
			}

			text, ok := readStopHookResponse(respFile, readyFile)
			if text != tc.expectedText {
				t.Errorf("text = %q, want %q", text, tc.expectedText)
			}
			if ok != tc.expectedOK {
				t.Errorf("ok = %v, want %v", ok, tc.expectedOK)
			}

			// Files should be cleaned up after reading only when both existed
			if tc.setupReady && tc.setupResp {
				if _, err := os.Stat(respFile); !os.IsNotExist(err) {
					t.Error("resp file should be removed after reading")
				}
				if _, err := os.Stat(readyFile); !os.IsNotExist(err) {
					t.Error("ready file should be removed after reading")
				}
			}
		})
	}
}

func TestReadStopHookResponse_EdgeCases(t *testing.T) {
	t.Run("ready file is directory", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// Create ready as a directory instead of a file
		if err := os.MkdirAll(readyFile, 0700); err != nil {
			t.Fatalf("create ready dir: %v", err)
		}
		if err := os.WriteFile(respFile, []byte("content"), 0600); err != nil {
			t.Fatalf("write resp file: %v", err)
		}

		// Should handle gracefully - os.Stat on a directory succeeds, but ReadFile
		// should still work
		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true even with ready as directory")
		}
		if text != "content" {
			t.Errorf("text = %q, want %q", text, "content")
		}
	})

	t.Run("resp file is directory", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// Create resp as a directory instead of a file
		if err := os.MkdirAll(respFile, 0700); err != nil {
			t.Fatalf("create resp dir: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		// Should handle gracefully - ReadFile on a directory fails
		text, ok := readStopHookResponse(respFile, readyFile)
		if ok {
			t.Error("expected ok=false when resp is a directory")
		}
		if text != "" {
			t.Errorf("text = %q, want empty string", text)
		}
	})

	t.Run("both files are directories", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		if err := os.MkdirAll(respFile, 0700); err != nil {
			t.Fatalf("create resp dir: %v", err)
		}
		if err := os.MkdirAll(readyFile, 0700); err != nil {
			t.Fatalf("create ready dir: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if ok {
			t.Error("expected ok=false when both are directories")
		}
		if text != "" {
			t.Errorf("text = %q, want empty string", text)
		}
	})

	t.Run("unicode content", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		content := "Hello 世界 🌍\nMore unicode: ● ❯ ✻ ✽"
		if err := os.WriteFile(respFile, []byte(content), 0600); err != nil {
			t.Fatalf("write resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true for unicode content")
		}
		if text != content {
			t.Errorf("text = %q, want %q", text, content)
		}
	})

	t.Run("very large content", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// Create a 100KB response
		largeContent := strings.Repeat("A", 100*1024)
		if err := os.WriteFile(respFile, []byte(largeContent), 0600); err != nil {
			t.Fatalf("write resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true for large content")
		}
		if text != largeContent {
			t.Errorf("text length = %d, want %d", len(text), len(largeContent))
		}
	})

	t.Run("consecutive calls cleanup", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// First call - files exist
		if err := os.WriteFile(respFile, []byte("first"), 0600); err != nil {
			t.Fatalf("write resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok || text != "first" {
			t.Errorf("first call: got (%q, %v), want (%q, true)", text, ok, "first")
		}

		// Second call - files should be gone
		text, ok = readStopHookResponse(respFile, readyFile)
		if ok {
			t.Error("second call: expected ok=false after cleanup")
		}
		if text != "" {
			t.Errorf("second call: text = %q, want empty", text)
		}
	})
}

// ── PTYManager Tests ───────────────────────────────────────────────────────────────

func TestTmuxMockFixtures(t *testing.T) {
	state := setupTmuxTest(t)
	manager := NewPTYManager()
	target := "telegram-bridge:t100-10"

	mockTmuxCommand(t, "capture-pane", tmuxCapturePaneOutput)
	screen, err := manager.CaptureScreen(target)
	if err != nil {
		t.Fatalf("CaptureScreen with fixture: %v", err)
	}
	if screen != tmuxCapturePaneOutput {
		t.Errorf("CaptureScreen = %q, want %q", screen, tmuxCapturePaneOutput)
	}

	tty, err := manager.PaneTTY(target)
	if err != nil {
		t.Fatalf("PaneTTY with fixture: %v", err)
	}
	if tty != strings.TrimSpace(tmuxPaneTTYOutput) {
		t.Errorf("PaneTTY = %q, want %q", tty, strings.TrimSpace(tmuxPaneTTYOutput))
	}
	if !manager.PaneAlive(target) {
		t.Error("PaneAlive = false, want true")
	}

	// A failed has-session response exercises the new-session fixture path.
	mockTmuxCommandResult(t, "has-session", "", 1)
	if err := manager.EnsureSession(); err != nil {
		t.Fatalf("EnsureSession with new-session fixture: %v", err)
	}

	// A failed command should be surfaced by the production method.
	mockTmuxCommandFailure(t, "capture-pane", "capture failed", 1)
	if _, err := manager.CaptureScreen(target); err == nil {
		t.Fatal("CaptureScreen succeeded for failed tmux fixture")
	}

	calls := state.tmuxCalls(t)
	if len(calls) < 5 {
		t.Fatalf("recorded %d tmux calls, want at least 5: %v", len(calls), calls)
	}
	if calls[0] != "capture-pane -t "+target+" -p -S -" {
		t.Errorf("first tmux call = %q, want capture-pane invocation", calls[0])
	}

	teardownTmuxTest(t, state)
}

func TestShellMock(t *testing.T) {
	state := setupTmuxTest(t)

	for command, response := range tmuxCommandFixtures() {
		command, response := command, response
		t.Run(command, func(t *testing.T) {
			got, err := state.shellExec(command, "-t", tmuxSessionName)
			if err != nil {
				t.Fatalf("mockShellExec(%q) returned error: %v", command, err)
			}
			want := append([]byte(response.stdout), response.stderr...)
			if string(got) != string(want) {
				t.Errorf("mockShellExec(%q) = %q, want %q", command, got, want)
			}
		})
	}

	t.Run("invalid session", func(t *testing.T) {
		mockTmuxCommandFailure(t, "has-session", "invalid session\n", 1)

		got, err := state.shellExec("has-session", "-t", "invalid-session")
		if err == nil {
			t.Fatal("mockShellExec for invalid session returned nil error")
		}
		if string(got) != "invalid session\n" {
			t.Errorf("invalid session output = %q, want %q", got, "invalid session\n")
		}
	})

	t.Run("pane not found", func(t *testing.T) {
		mockTmuxCommandFailure(t, "capture-pane", "pane not found\n", 1)

		got, err := state.shellExec("capture-pane", "-t", "telegram-bridge:missing-pane")
		if err == nil {
			t.Fatal("mockShellExec for missing pane returned nil error")
		}
		if string(got) != "pane not found\n" {
			t.Errorf("missing pane output = %q, want %q", got, "pane not found\n")
		}

		if _, err := NewPTYManager().CaptureScreen("telegram-bridge:missing-pane"); err == nil {
			t.Fatal("CaptureScreen succeeded for missing pane fixture")
		}
	})
}

func TestNewPTYManager(t *testing.T) {
	pm := NewPTYManager()
	if pm == nil {
		t.Fatal("NewPTYManager returned nil")
	}
	if pm.idleTimers == nil {
		t.Error("idleTimers map not initialized")
	}
}

func TestPTYManager_ScheduleIdleKill(t *testing.T) {
	pm := NewPTYManager()
	paneTarget := "test-session:123"

	killed := false
	pm.ScheduleIdleKill(paneTarget, 50*time.Millisecond, func() {
		killed = true
	})

	// Timer should be set
	if _, exists := pm.idleTimers[paneTarget]; !exists {
		t.Error("timer not set for pane")
	}

	// Wait for kill
	time.Sleep(150 * time.Millisecond)

	if !killed {
		t.Error("kill callback was not invoked")
	}

	// Timer should be removed
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if _, exists := pm.idleTimers[paneTarget]; exists {
		t.Error("timer not removed after firing")
	}
}

func TestPTYManager_ScheduleIdleKill_Cancel(t *testing.T) {
	pm := NewPTYManager()
	paneTarget := "test-session:456"

	killed := false
	pm.ScheduleIdleKill(paneTarget, 50*time.Millisecond, func() {
		killed = true
	})

	// Cancel immediately
	pm.CancelIdleTimer(paneTarget)

	// Wait longer than the timer
	time.Sleep(150 * time.Millisecond)

	if killed {
		t.Error("kill callback should not fire after cancellation")
	}

	// Timer should be removed
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if _, exists := pm.idleTimers[paneTarget]; exists {
		t.Error("timer not removed after cancellation")
	}
}

func TestPTYManager_ScheduleIdleKill_Replace(t *testing.T) {
	pm := NewPTYManager()
	paneTarget := "test-session:789"

	firstKilled := false
	secondKilled := false

	pm.ScheduleIdleKill(paneTarget, 50*time.Millisecond, func() {
		firstKilled = true
	})

	// Replace with new timer
	pm.ScheduleIdleKill(paneTarget, 200*time.Millisecond, func() {
		secondKilled = true
	})

	// First kill time
	time.Sleep(100 * time.Millisecond)
	if firstKilled {
		t.Error("first timer should be cancelled by replacement")
	}

	// Second kill time
	time.Sleep(150 * time.Millisecond)
	if !secondKilled {
		t.Error("second timer should fire")
	}
}

func TestPTYManager_CancelIdleTimer_NoTimer(t *testing.T) {
	pm := NewPTYManager()
	// Should not panic
	pm.CancelIdleTimer("nonexistent:pane")
}

// ── SnapshotSessionFiles Tests ───────────────────────────────────────────────────────

func TestSnapshotSessionFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create test directory structure
	cwdHash := strings.ReplaceAll("test/path", "/", "-")
	projectDir := filepath.Join(home, ".claude", "projects", cwdHash)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	// Create some session files
	sessions := []string{"session-1", "session-2", "session-3"}
	for _, sess := range sessions {
		path := filepath.Join(projectDir, sess+".jsonl")
		if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
			t.Fatalf("write session file: %v", err)
		}
	}

	// Create other files (should be ignored)
	otherFile := filepath.Join(projectDir, "other.txt")
	if err := os.WriteFile(otherFile, []byte("data"), 0600); err != nil {
		t.Fatalf("write other file: %v", err)
	}

	snap := SnapshotSessionFiles("test/path")

	// Should have 3 session files
	if len(snap) != 3 {
		t.Errorf("got %d session files, want 3", len(snap))
	}

	// All expected sessions should be present
	for _, sess := range sessions {
		if !snap[sess] {
			t.Errorf("snapshot missing session %q", sess)
		}
	}
}

func TestSnapshotSessionFiles_NonexistentDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	snap := SnapshotSessionFiles("/nonexistent/path")

	// Should return empty map, not error
	if len(snap) != 0 {
		t.Errorf("got %d session files, want 0 for nonexistent dir", len(snap))
	}
}

// ── FindNewSession Tests ──────────────────────────────────────────────────────────────

func TestFindNewSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "test/path"
	cwdHash := strings.ReplaceAll(cwd, "/", "-")
	projectDir := filepath.Join(home, ".claude", "projects", cwdHash)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	// Create initial snapshot with existing sessions
	existingSession := "existing-session"
	path := filepath.Join(projectDir, existingSession+".jsonl")
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatalf("write existing session: %v", err)
	}

	snap := SnapshotSessionFiles(cwd)
	if len(snap) != 1 {
		t.Fatalf("initial snapshot has %d files, want 1", len(snap))
	}

	// Add new session
	newSessionID := "new-session-abc123"
	newPath := filepath.Join(projectDir, newSessionID+".jsonl")
	if err := os.WriteFile(newPath, []byte("new data"), 0600); err != nil {
		t.Fatalf("write new session: %v", err)
	}

	found, err := FindNewSession(snap, cwd)
	if err != nil {
		t.Fatalf("FindNewSession: %v", err)
	}

	if found != newSessionID {
		t.Errorf("found session %q, want %q", found, newSessionID)
	}
}

func TestFindNewSession_NoNewSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "test/path"
	cwdHash := strings.ReplaceAll(cwd, "/", "-")
	projectDir := filepath.Join(home, ".claude", "projects", cwdHash)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	// Create a session
	sess := "session-1"
	path := filepath.Join(projectDir, sess+".jsonl")
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	snap := SnapshotSessionFiles(cwd)

	// Don't add any new sessions

	_, err := FindNewSession(snap, cwd)
	if err == nil {
		t.Error("expected error when no new session found")
	}
}

func TestFindNewSession_FilterNonJsonl(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "test/path"
	cwdHash := strings.ReplaceAll(cwd, "/", "-")
	projectDir := filepath.Join(home, ".claude", "projects", cwdHash)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}

	snap := SnapshotSessionFiles(cwd)

	// Add non-jsonl file (should be ignored)
	txtFile := filepath.Join(projectDir, "not-session.txt")
	if err := os.WriteFile(txtFile, []byte("data"), 0600); err != nil {
		t.Fatalf("write txt file: %v", err)
	}

	// Add actual session
	sess := "real-session"
	sessPath := filepath.Join(projectDir, sess+".jsonl")
	if err := os.WriteFile(sessPath, []byte("data"), 0600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	found, err := FindNewSession(snap, cwd)
	if err != nil {
		t.Fatalf("FindNewSession: %v", err)
	}

	if found != "real-session" {
		t.Errorf("found session %q, want real-session", found)
	}
}

// ── PrepareRespFile Tests ────────────────────────────────────────────────────────────

func TestPrepareRespFile(t *testing.T) {
	paneName := "test-pane-123"

	path, err := prepareRespFile(paneName)
	if err != nil {
		t.Fatalf("prepareRespFile: %v", err)
	}

	// Path should be in temp dir
	if !strings.Contains(path, "telegram-bridge-resp") {
		t.Errorf("path %q does not contain expected dir", path)
	}
	if !strings.HasSuffix(path, paneName+".resp") {
		t.Errorf("path %q does not end with %s.resp", path, paneName)
	}

	// Directory should exist
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Errorf("resp dir %q does not exist", dir)
	}
}

func TestPrepareRespFile_RemovesStale(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	paneName := "test-pane-456"
	dir := filepath.Join(tempDir, "telegram-bridge-resp")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create stale files
	staleResp := filepath.Join(dir, paneName+".resp")
	staleReady := filepath.Join(dir, paneName+".resp.ready")
	staleTmp := filepath.Join(dir, paneName+".resp.tmp")

	os.WriteFile(staleResp, []byte("old"), 0600)
	os.WriteFile(staleReady, []byte("ready"), 0600)
	os.WriteFile(staleTmp, []byte("tmp"), 0600)

	// Prepare should remove them
	path, err := prepareRespFile(paneName)
	if err != nil {
		t.Fatalf("prepareRespFile: %v", err)
	}

	// Stale files should be gone
	for _, stale := range []string{staleResp, staleReady, staleTmp} {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("stale file %q should be removed", stale)
		}
	}

	// Returned path should match expected
	expectedPath := filepath.Join(dir, paneName+".resp")
	if path != expectedPath {
		t.Errorf("path = %q, want %q", path, expectedPath)
	}
}

func TestPrepareRespFile_MkdirFailure(t *testing.T) {
	// This test verifies that prepareRespFile handles mkdir failures gracefully
	// We can't easily test actual permission errors without running as root,
	// but we can test that the error is surfaced
	paneName := "test-pane-error"

	// Create a directory with the same name as the file we want to create
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	// Force TMPDIR to be a file instead of directory
	if err := os.RemoveAll(tempDir); err != nil {
		t.Fatalf("remove tempdir: %v", err)
	}
	if err := os.WriteFile(tempDir, []byte("not a directory"), 0600); err != nil {
		t.Fatalf("write file as tempdir: %v", err)
	}

	_, err := prepareRespFile(paneName)
	if err == nil {
		t.Error("expected error when TMPDIR is not a directory")
	}
}

func TestPrepareRespFile_Idempotent(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	paneName := "test-pane-idempotent"

	// Call prepare multiple times
	path1, err1 := prepareRespFile(paneName)
	if err1 != nil {
		t.Fatalf("first prepareRespFile: %v", err1)
	}

	path2, err2 := prepareRespFile(paneName)
	if err2 != nil {
		t.Fatalf("second prepareRespFile: %v", err2)
	}

	// Should return same path
	if path1 != path2 {
		t.Errorf("paths differ: %q vs %q", path1, path2)
	}

	// Directory should exist
	dir := filepath.Dir(path1)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("directory should exist after prepare")
	}
}

func TestWaitForResponse_TimeoutScenarios(t *testing.T) {
	tests := []struct {
		name          string
		screenContent string
		setupHook     bool
		hookContent   string
		expectTimeout bool
		expectOK      bool
	}{
		{
			name:          "timeout waiting for response start",
			screenContent: "no bullet here\n❯",
			setupHook:     false,
			expectTimeout: true,
			expectOK:      false,
		},
		{
			name: "stop hook arrives before timeout",
			// Complete screen: the hook file is only consulted once the
			// screen signals completion, so an incomplete screen would hit
			// the context deadline before the hook check ever runs.
			screenContent: "● Response\n❯",
			setupHook:     true,
			hookContent:   "hook response",
			expectTimeout: false,
			expectOK:      true,
		},
		{
			name:          "pty completes before timeout",
			screenContent: "● Response\n❯",
			setupHook:     false,
			expectTimeout: false,
			expectOK:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupTmuxTest(t)

			// Set screen content
			mockTmuxCommand(t, "capture-pane", tc.screenContent)

			paneTarget := "telegram-bridge:timeout-test"
			paneName := "timeout-test"
			respFile := bridgeRespFile(paneName)
			readyFile := respFile + ".ready"
			t.Cleanup(func() {
				os.Remove(respFile)
				os.Remove(readyFile)
			})

			// If hook is set up, deliver it asynchronously
			if tc.setupHook {
				go func() {
					time.Sleep(100 * time.Millisecond)
					_ = os.WriteFile(respFile, []byte(tc.hookContent), 0600)
					_ = os.WriteFile(readyFile, []byte("ready"), 0600)
				}()
			}

			// Use a short timeout context
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			text, err := NewPTYManager().WaitForResponse(ctx, paneTarget, "", nil)

			if tc.expectTimeout {
				if err == nil {
					t.Error("expected timeout error, got nil")
				} else if !strings.Contains(err.Error(), "timeout") && err != context.DeadlineExceeded {
					t.Errorf("expected timeout error, got: %v", err)
				}
			} else if tc.expectOK {
				if err != nil {
					t.Errorf("expected success, got error: %v", err)
				}
				if tc.setupHook && text != tc.hookContent {
					t.Errorf("expected hook content %q, got %q", tc.hookContent, text)
				}
			}
		})
	}
}

func TestWaitForResponse_ContextCancellation(t *testing.T) {
	setupTmuxTest(t)

	// Screen without bullet to keep it waiting
	mockTmuxCommand(t, "capture-pane", "waiting for response\n❯")

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel immediately
	cancel()

	paneTarget := "telegram-bridge:cancel-test"

	text, err := NewPTYManager().WaitForResponse(ctx, paneTarget, "", nil)
	if err == nil {
		t.Error("expected context canceled error, got nil")
	} else if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected context canceled error, got: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty text on cancel, got %q", text)
	}
}

func TestWaitForResponse_PaneDied(t *testing.T) {
	setupTmuxTest(t)

	// Make has-session fail to simulate pane death
	mockTmuxCommandResult(t, "has-session", "", 1)
	mockTmuxCommand(t, "capture-pane", "● Response\n❯")

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	paneTarget := "telegram-bridge:dead-pane"

	text, err := NewPTYManager().WaitForResponse(ctx, paneTarget, "", nil)
	if err == nil {
		t.Error("expected pane died error, got nil")
	} else if !strings.Contains(err.Error(), "pane died") {
		t.Errorf("expected pane died error, got: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty text on pane death, got %q", text)
	}
}

func TestWaitForResponse_BaselineBulletCount(t *testing.T) {
	setupTmuxTest(t)

	// Screen with bullets in history before our injection
	preInjectScreen := "● Old response 1\n● Old response 2\n❯"

	// New response will add one more bullet
	mockTmuxCommand(t, "capture-pane", "● Old response 1\n● Old response 2\n❯\n● New response\n❯")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	paneTarget := "telegram-bridge:baseline-test"

	text, err := NewPTYManager().WaitForResponse(ctx, paneTarget, preInjectScreen, nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	// Should extract the new response, not the old ones
	if text != "New response" {
		t.Errorf("expected new response, got %q", text)
	}
}

func TestWaitForResponse_MultipleBulletsInHistory(t *testing.T) {
	setupTmuxTest(t)

	// Many bullets in history
	preInjectScreen := strings.Repeat("● Old line\n", 10) + "❯"

	// New response
	newScreen := preInjectScreen + "● New response\n❯"
	mockTmuxCommand(t, "capture-pane", newScreen)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	paneTarget := "telegram-bridge:multi-bullet"

	text, err := NewPTYManager().WaitForResponse(ctx, paneTarget, preInjectScreen, nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if text != "New response" {
		t.Errorf("expected new response, got %q", text)
	}
}

func TestWaitForResponse_StopHookCleanup(t *testing.T) {
	setupTmuxTest(t)

	mockTmuxCommand(t, "capture-pane", "● PTY response\n❯")

	paneTarget := "telegram-bridge:cleanup-test"
	paneName := "cleanup-test"
	respFile := bridgeRespFile(paneName)
	readyFile := respFile + ".ready"

	// Write the files asynchronously: WaitForResponse clears stale files at
	// entry, so files created before the call would never be observed.
	go func() {
		time.Sleep(25 * time.Millisecond)
		_ = os.WriteFile(respFile, []byte("hook response"), 0600)
		_ = os.WriteFile(readyFile, []byte("ready"), 0600)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	text, err := NewPTYManager().WaitForResponse(ctx, paneTarget, "", nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if text != "hook response" {
		t.Errorf("expected hook response, got %q", text)
	}

	// Files should be cleaned up after reading
	if _, err := os.Stat(respFile); !os.IsNotExist(err) {
		t.Error("resp file should be removed after WaitForResponse")
	}
	if _, err := os.Stat(readyFile); !os.IsNotExist(err) {
		t.Error("ready file should be removed after WaitForResponse")
	}
}

func TestWaitForResponseWithSource_SourceTracking(t *testing.T) {
	tests := []struct {
		name           string
		setupHook      bool
		hookContent    string
		screenContent  string
		expectedSource ResponseSource
	}{
		{
			name:           "stop hook source",
			setupHook:      true,
			hookContent:    "hook response",
			screenContent:  "● PTY response\n❯",
			expectedSource: ResponseSourceStopHook,
		},
		{
			name:           "pty fallback source",
			setupHook:      false,
			hookContent:    "",
			screenContent:  "● PTY response\n❯",
			expectedSource: ResponseSourcePTY,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupTmuxTest(t)
			mockTmuxCommand(t, "capture-pane", tc.screenContent)

			paneTarget := "telegram-bridge:source-test"
			paneName := "source-test"
			respFile := bridgeRespFile(paneName)
			readyFile := respFile + ".ready"
			t.Cleanup(func() {
				os.Remove(respFile)
				os.Remove(readyFile)
			})

			if tc.setupHook {
				go func() {
					time.Sleep(50 * time.Millisecond)
					_ = os.WriteFile(respFile, []byte(tc.hookContent), 0600)
					_ = os.WriteFile(readyFile, []byte("ready"), 0600)
				}()
			}

			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			text, source, err := NewPTYManager().WaitForResponseWithSource(ctx, paneTarget, "", nil)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if source != tc.expectedSource {
				t.Errorf("source = %v, want %v", source, tc.expectedSource)
			}
			if tc.setupHook && text != tc.hookContent {
				t.Errorf("text = %q, want %q", text, tc.hookContent)
			}
		})
	}
}

func TestWaitForResponse_ChunkStreaming(t *testing.T) {
	setupTmuxTest(t)

	// Progressive screen updates showing response growth
	screens := []string{
		"● start",
		"● start\nmore",
		"● start\nmore\nfinal",
		"● start\nmore\nfinal\n❯",
	}

	screenIndex := 0
	mockTmuxCommand(t, "capture-pane", screens[0])

	// Update screen on each capture
	updateScreen := func() {
		if screenIndex < len(screens)-1 {
			screenIndex++
			mockTmuxCommand(t, "capture-pane", screens[screenIndex])
		}
	}

	paneTarget := "telegram-bridge:chunk-test"

	var chunks []string
	onChunk := func(text string) {
		chunks = append(chunks, text)
		// Trigger screen update after receiving chunk
		updateScreen()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	text, err := NewPTYManager().WaitForResponse(ctx, paneTarget, "", onChunk)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Should have received multiple chunks as response grew
	if len(chunks) == 0 {
		t.Error("expected at least one chunk")
	}

	// Final text should match expected
	expectedText := "start\nmore\nfinal"
	if text != expectedText {
		t.Errorf("final text = %q, want %q", text, expectedText)
	}

	// Last chunk should match final text
	if len(chunks) > 0 && chunks[len(chunks)-1] != expectedText {
		t.Errorf("last chunk = %q, want %q", chunks[len(chunks)-1], expectedText)
	}
}

// ── Stop-Hook File Reading Tests ────────────────────────────────────────────────────────

func TestStopHook_EmptyFiles(t *testing.T) {
	t.Run("empty response file with ready marker", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		if err := os.WriteFile(respFile, []byte(""), 0600); err != nil {
			t.Fatalf("write empty resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true for empty file with ready marker")
		}
		if text != "" {
			t.Errorf("expected empty string, got %q", text)
		}
	})

	t.Run("zero-byte ready file", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		if err := os.WriteFile(respFile, []byte("response"), 0600); err != nil {
			t.Fatalf("write resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte(""), 0600); err != nil {
			t.Fatalf("write empty ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true even with zero-byte ready file")
		}
		if text != "response" {
			t.Errorf("expected %q, got %q", "response", text)
		}
	})
}

func TestStopHook_MalformedData(t *testing.T) {
	t.Run("binary content", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// Write some binary data
		binaryData := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0xFD}
		if err := os.WriteFile(respFile, binaryData, 0600); err != nil {
			t.Fatalf("write binary resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true for binary content")
		}
		if text != string(binaryData) {
			t.Errorf("binary data not preserved correctly")
		}
	})

	t.Run("mixed newlines and carriage returns", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		mixedContent := "line1\nline2\r\nline3\rline4\n"
		if err := os.WriteFile(respFile, []byte(mixedContent), 0600); err != nil {
			t.Fatalf("write mixed line endings: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true for mixed line endings")
		}
		// Trailing newlines should be trimmed
		expected := "line1\nline2\r\nline3\rline4"
		if text != expected {
			t.Errorf("got %q, want %q", text, expected)
		}
	})

	t.Run("only whitespace", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		whitespaceContent := "   \t\n  \t  \n  "
		if err := os.WriteFile(respFile, []byte(whitespaceContent), 0600); err != nil {
			t.Fatalf("write whitespace: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true for whitespace-only content")
		}
		// strings.TrimRight only removes trailing newlines, not spaces/tabs
		expected := "   \t\n  \t  \n  "
		if text != expected {
			t.Errorf("got %q, want %q", text, expected)
		}
	})

	t.Run("special characters", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		specialContent := "null: \x00, bell: \a, escape: \x1b\n"
		if err := os.WriteFile(respFile, []byte(specialContent), 0600); err != nil {
			t.Fatalf("write special chars: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true for special characters")
		}
		expected := "null: \x00, bell: \a, escape: \x1b"
		if text != expected {
			t.Errorf("got %q, want %q", text, expected)
		}
	})
}

func TestStopHook_FileSystemErrors(t *testing.T) {
	t.Run("permission denied on resp file", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("running as root, permissions test not applicable")
		}

		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// Create files with no read permission
		if err := os.WriteFile(respFile, []byte("content"), 0600); err != nil {
			t.Fatalf("write resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		// Remove read permissions
		if err := os.Chmod(respFile, 0000); err != nil {
			t.Fatalf("chmod resp file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if ok {
			t.Error("expected ok=false when resp file unreadable")
		}
		if text != "" {
			t.Errorf("expected empty string, got %q", text)
		}
	})

	t.Run("symlink to resp file", func(t *testing.T) {
		tempDir := t.TempDir()
		realRespFile := filepath.Join(tempDir, "real.resp")
		symlinkRespFile := filepath.Join(tempDir, "symlink.resp")
		readyFile := symlinkRespFile + ".ready"

		// Create real file and symlink
		if err := os.WriteFile(realRespFile, []byte("real content"), 0600); err != nil {
			t.Fatalf("write real resp file: %v", err)
		}
		if err := os.Symlink(realRespFile, symlinkRespFile); err != nil {
			t.Fatalf("create symlink: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(symlinkRespFile, readyFile)
		if !ok {
			t.Error("expected ok=true for symlinked resp file")
		}
		if text != "real content" {
			t.Errorf("got %q, want %q", text, "real content")
		}
	})

	t.Run("broken symlink to resp file", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// Create broken symlink
		if err := os.Symlink("/nonexistent/file", respFile); err != nil {
			t.Fatalf("create broken symlink: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if ok {
			t.Error("expected ok=false for broken symlink")
		}
		if text != "" {
			t.Errorf("expected empty string, got %q", text)
		}
	})
}

func TestStopHook_ConcurrentAccess(t *testing.T) {
	t.Run("multiple goroutines reading same files", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// Create files
		content := "shared content"
		if err := os.WriteFile(respFile, []byte(content), 0600); err != nil {
			t.Fatalf("write resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		// Launch multiple goroutines to read simultaneously
		results := make(chan struct {
			text string
			ok   bool
		}, 10)

		for i := 0; i < 10; i++ {
			go func() {
				text, ok := readStopHookResponse(respFile, readyFile)
				results <- struct {
					text string
					ok   bool
				}{text, ok}
			}()
		}

		// Collect results
		successCount := 0
		for i := 0; i < 10; i++ {
			result := <-results
			if result.ok && result.text == content {
				successCount++
			}
		}

		// At least one should succeed (the first reader)
		if successCount == 0 {
			t.Error("expected at least one successful read")
		}
		// After first read, files are cleaned up so subsequent reads should fail
		if successCount > 1 {
			t.Logf("warning: %d goroutines reported success (expected 1 due to cleanup)", successCount)
		}
	})

	t.Run("read while files are being deleted", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// Create files
		if err := os.WriteFile(respFile, []byte("content"), 0600); err != nil {
			t.Fatalf("write resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		// Start deletion in background
		go func() {
			time.Sleep(10 * time.Millisecond)
			os.Remove(respFile)
			os.Remove(readyFile)
		}()

		// Try to read immediately - may succeed or fail depending on timing
		text, ok := readStopHookResponse(respFile, readyFile)
		// Either outcome is acceptable due to race condition
		_ = text
		_ = ok
		// Test just ensures it doesn't panic or hang
	})
}

func TestStopHook_LargeFiles(t *testing.T) {
	t.Run("file larger than typical pipe buffer", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// Create 1MB content (larger than typical 64KB pipe buffer)
		largeContent := strings.Repeat("A", 1024*1024)
		if err := os.WriteFile(respFile, []byte(largeContent), 0600); err != nil {
			t.Fatalf("write large resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true for large file")
		}
		if text != largeContent {
			t.Errorf("large content not preserved, got %d bytes want %d", len(text), len(largeContent))
		}
	})

	t.Run("file with many newlines", func(t *testing.T) {
		tempDir := t.TempDir()
		respFile := filepath.Join(tempDir, "test.resp")
		readyFile := respFile + ".ready"

		// Create content with 10000 lines
		var lines []string
		for i := 0; i < 10000; i++ {
			lines = append(lines, fmt.Sprintf("line %d", i))
		}
		content := strings.Join(lines, "\n")
		if err := os.WriteFile(respFile, []byte(content), 0600); err != nil {
			t.Fatalf("write multi-line resp file: %v", err)
		}
		if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}

		text, ok := readStopHookResponse(respFile, readyFile)
		if !ok {
			t.Error("expected ok=true for multi-line file")
		}
		if text != content {
			t.Errorf("multi-line content not preserved, got %d lines want %d", strings.Count(text, "\n"), strings.Count(content, "\n"))
		}
	})
}

// ── Response Detection Edge Cases ─────────────────────────────────────────────────────

func TestResponseDetection_EdgeCaseScreens(t *testing.T) {
	tests := []struct {
		name     string
		screen   string
		expected bool
		reason   string
	}{
		{
			name:     "bullet at very end",
			screen:   "some text\n●",
			expected: false,
			reason:   "no prompt after bullet",
		},
		{
			name:     "prompt at very start",
			screen:   "❯\n● response",
			expected: false,
			reason:   "prompt appears before bullet",
		},
		{
			name:     "only bullet character",
			screen:   "●",
			expected: false,
			reason:   "no prompt after lone bullet",
		},
		{
			name:     "only prompt character",
			screen:   "❯",
			expected: false,
			reason:   "no bullet before prompt",
		},
		{
			name:     "bullet and prompt same line",
			screen:   "● response ❯",
			expected: false,
			reason:   "prompt must appear on a line after the bullet line",
		},
		{
			name:     "bullet in middle of timing line",
			screen:   "✻ Brewed ● response\n❯",
			expected: true,
			reason:   "first ● is timing line, but check should find it",
		},
		{
			name:     "multiple bullets last with active indicator",
			screen:   "● First\n● Second\nReading 1 file…\n❯",
			expected: false,
			reason:   "active reading after last bullet blocks completion",
		},
		{
			name:     "cooking with token count",
			screen:   "● Response\n✽ Cooking… (10s · ↑ 5000 tokens)\n❯",
			expected: false,
			reason:   "cooking line is active indicator",
		},
		{
			name:     "empty lines around bullet",
			screen:   "\n\n● Response\n\n\n❯",
			expected: true,
			reason:   "empty lines don't affect completion detection",
		},
		{
			name:     "very long single line between bullet and prompt",
			screen:   "● Response\n" + strings.Repeat("A", 10000) + "\n❯",
			expected: true,
			reason:   "long line doesn't affect completion detection",
		},
		{
			name:     "prompt with additional text",
			screen:   "● Response\n❯ continue typing...",
			expected: true,
			reason:   "prompt with text still indicates completion",
		},
		{
			name:     "timing line without ellipsis",
			screen:   "● Response\n✻ Done\n❯",
			expected: false,
			reason:   "any ✻-prefixed line is a timing indicator, with or without ellipsis",
		},
		{
			name:     "asterisk timing without parens",
			screen:   "● Response\n* Working\n❯",
			expected: true,
			reason:   "asterisk without ellipsis or parentheses is not a timing line",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := responseComplete(tc.screen)
			if got != tc.expected {
				t.Errorf("responseComplete() = %v, want %v (reason: %s)", got, tc.expected, tc.reason)
			}
		})
	}
}

func TestResponseDetection_ActiveIndicatorVariants(t *testing.T) {
	tests := []struct {
		name     string
		screen   string
		expected bool
	}{
		{
			name:     "standard cooking indicator",
			screen:   "● Response\n✽ Cooking…\n❯",
			expected: false,
		},
		{
			name:     "cooking with time",
			screen:   "● Response\n✽ Cooking… (5s)\n❯",
			expected: false,
		},
		{
			name:     "cooking with token arrow",
			screen:   "● Response\n✽ Cooking… (↑ 1000 tokens)\n❯",
			expected: false,
		},
		{
			name:     "contemplating indicator",
			screen:   "● Response\n✢ Contemplating…\n❯",
			expected: false,
		},
		{
			name:     "brewed with time",
			screen:   "● Response\n✻ Brewed for 2s\n❯",
			expected: false,
		},
		{
			name:     "asterisk cooking form",
			screen:   "● Response\n* Cooking… (10s)\n❯",
			expected: false,
		},
		{
			name:     "asterisk with three dots",
			screen:   "● Response\n* Processing...\n❯",
			expected: false,
		},
		{
			name:     "reading one file",
			screen:   "● Response\nReading 1 file…\n❯",
			expected: false,
		},
		{
			name:     "reading many files",
			screen:   "● Response\nReading 12345 files…\n❯",
			expected: false,
		},
		{
			name:     "reading with ctrl+o hint",
			screen:   "● Response\nReading 5 files… (ctrl+o to expand)\n❯",
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := responseComplete(tc.screen)
			if got != tc.expected {
				t.Errorf("responseComplete() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestResponseDetection_TimingLinePatterns(t *testing.T) {
	tests := []struct {
		line     string
		expected bool
	}{
		{"✻ Brewed for 5s", true},
		{"✻ Brewed for 5s ", true},
		{"✢ Contemplating…", true},
		{"✽ Cooking… (5s · ↑ 1000 tokens)", true},
		{"✽ Cooking… (5s)", true},
		{"* Word… (2s · ↑ 100 tokens)", true},
		{"* Working... (10s)", true},
		{"* Process started", false}, // No ellipsis or parens after the verb
		{"✻ Reading files", true},
		{"✻ Done", true},
		{"✻", true},
		{"✢", true},
		{"✽", true},
		{"* not a timing line", false},
		{"*no space", false},
		{"regular text", false},
		{"", false},
		{"*", false},
		{"✻ not timing", true},    // Any ✻ prefix marks a timing line
		{"* Normal text", false},  // Without ellipsis or parens
		{"✻ brewed for 5s", true}, // Prefix match regardless of case
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			got := isTimingLine(tc.line)
			if got != tc.expected {
				t.Errorf("isTimingLine(%q) = %v, want %v", tc.line, got, tc.expected)
			}
		})
	}
}

func TestResponseDetection_ToolCallPatterns(t *testing.T) {
	tests := []struct {
		line     string
		expected bool
	}{
		// Valid tool calls
		{"Bash(cmd)", true},
		{"Read(path)", true},
		{"Write(file)", true},
		{"Edit(path)", true},
		{"WebSearch(query)", true},
		{"LSP(operation)", true},
		{"Tool123_name(arg)", true},
		{"MyTool(arg)", true},
		{"Agent(subtask)", true},
		{"Skill(invoke)", true},
		{"Edit(file_path)", true},
		{"NotebookEdit(notebook_path)", true},
		{"TaskCreate(subject)", true},
		{"ReportFindings(findings)", true},

		// With spinner prefix (non-ASCII braille range U+2800–U+28FF)
		{"⠋ Bash(cmd)", true},
		{"⠁⠁⠁ Read(file)", true},
		{"⣶ Edit(path)", true},
		{"⠦ WebSearch(query)", true},
		{"⠼ Agent(task)", true},

		// Invalid - not starting with capital letter
		{"bash(cmd)", false},
		{"read(path)", false},
		{"123tool(arg)", false},

		// Invalid - no parentheses
		{"Bash", false},
		{"Read", false},
		{"Bash cmd", false},

		// Empty parentheses still match the Tool(args) shape
		{"Bash()", true},

		// Invalid - special chars in name
		{"Bash-Test(cmd)", false},
		{"Tool.Name(arg)", false},
		{"Tool Name(arg)", false},

		// Edge cases
		{"", false},
		{"()", false},
		{"Bash)(", false},
		{"Bash(cmd)(extra)", true}, // matches on the first paren group; trailing text is ignored

		// Valid - numbers in name
		{"Tool123(arg)", true},
		{"Tool0Name(arg)", true},

		// Valid - underscores
		{"Tool_Name(arg)", true},
		{"My_Tools_Name(arg)", true},

		// Mixed spinner and tool call
		{"⠋⠁⠼ Bash(cmd)", true},
		{"⠋ ⠁ Bash(cmd)", false}, // only one contiguous non-ASCII run is stripped; a second spinner after the space fails the capital-letter check
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			got := isToolCallLine(tc.line)
			if got != tc.expected {
				t.Errorf("isToolCallLine(%q) = %v, want %v", tc.line, got, tc.expected)
			}
		})
	}
}

func TestResponseDetection_ActiveProgressPatterns(t *testing.T) {
	tests := []struct {
		line     string
		expected bool
	}{
		{"Reading 1 file…", true},
		{"Reading 2 file…", true},
		{"Reading 100 files…", true},
		{"Reading 1 file… (ctrl+o to expand)", true},
		{"Reading 99999 files… (ctrl+o to expand)", true},
		{"Reading", false},
		{"Reading something else", false},
		{"reading 1 file…", false}, // Case sensitive
		{"READING 1 file…", false}, // All caps
		{"", false},
		{"Reading 1 file", true},    // Ellipsis is not required — prefix + "file" matches
		{"Reading 1 file...", true}, // Three dots still contain "file"
		{"Reading 0 files…", true},  // Zero is valid
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			got := isActiveProgressLine(tc.line)
			if got != tc.expected {
				t.Errorf("isActiveProgressLine(%q) = %v, want %v", tc.line, got, tc.expected)
			}
		})
	}
}

func TestResponseDetection_UIChromePatterns(t *testing.T) {
	tests := []struct {
		line     string
		expected bool
	}{
		{"──────────────────", true},
		{"══════════════════", true},
		{"━━━━━━━━━━━━━━━━", true},
		{"───────────────", true},
		{"─", true},
		{"══", true},
		{"━", true},
		{"────────text────", false}, // Has text mixed in
		{"text", false},
		{"", false},
		{"=", false},
		{"-", true},  // ASCII hyphen is an accepted separator char
		{"──", true}, // Only valid chars
		{"════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════", true}, // Very long line
		{"────────────────── ", false}, // Has space at end
		{" ──────────────────", false}, // Has space at start
		{"\t──────────", false},        // Has tab
		{"──────────\t", false},        // Has tab
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			got := isUIChrome(tc.line)
			if got != tc.expected {
				t.Errorf("isUIChrome(%q) = %v, want %v", tc.line, got, tc.expected)
			}
		})
	}
}

func TestResponseExtraction_ComplexScreens(t *testing.T) {
	tests := []struct {
		name     string
		screen   string
		expected string
	}{
		{
			name:     "response with all filterable line types",
			screen:   "pre\n● Response text\n✻ Brewed for 2s\nBash(cmd)\n⎿ output\n──────────\nReal content\n❯",
			expected: "Response text\nReal content",
		},
		{
			name:     "multiple bullets extracts from last",
			screen:   "● First response\n❯\n● Second response\nmore\n❯",
			expected: "Second response\nmore",
		},
		{
			name:     "nested tool calls",
			screen:   "● Text\nBash(outer)\n⠋ Read(inner)\n⎿ result\n❯",
			expected: "Text",
		},
		{
			name:     "reading progress mixed in",
			screen:   "● Start\nContent here\nReading 5 files… (ctrl+o to expand)\n❯",
			expected: "Start\nContent here",
		},
		{
			name:     "all timing variants",
			screen:   "● Text\n✻ Brewed\n✢ Contemplating\n✽ Cooking…\n❯",
			expected: "Text",
		},
		{
			name:     "preserves internal structure",
			screen:   "● Header\n  Indented line\n\tTabbed line\nNormal\n❯",
			expected: "Header\n  Indented line\n\tTabbed line\nNormal",
		},
		{
			name:     "unicode in response",
			screen:   "● 你好世界 🌍\nMore emoji: 🎉🚀\n❯",
			expected: "你好世界 🌍\nMore emoji: 🎉🚀",
		},
		{
			name:     "empty lines preserved",
			screen:   "● Line 1\n\nLine 3\n\n\n❯",
			expected: "Line 1\n\nLine 3", // trailing blank lines are trimmed by the final TrimSpace
		},
		{
			name:     "bullet with no prompt extracts all",
			screen:   "● Start\nMiddle\nEnd",
			expected: "Start\nMiddle\nEnd",
		},
		{
			name:     "prompt stops extraction mid-response",
			screen:   "● Part 1\n❯\n● Part 2\n❯",
			expected: "Part 2", // extraction anchors on the last ●, ignoring earlier prompts
		},
		{
			name:     "complex tool output",
			screen:   "● Text\n⠋ Bash(complex command with 'quotes' and \"double quotes\")\n⎿ Multiline\noutput\nhere\n❯",
			expected: "Text\noutput\nhere", // only the ⎿-prefixed line is filtered; continuation lines survive
		},
		{
			name:     "spaced timing indicators",
			screen:   "● Response\n  ✻ Brewed for 2s  \n  ❯",
			expected: "Response",
		},
		{
			name:     "mix of asterisk forms",
			screen:   "● Text\n* Word… (2s)\n* Process started\n❯",
			expected: "Text\n* Process started", // "* Process started" has no ellipsis/parens, so it is response text
		},
		{
			name:     "reading not ellipsis preserved",
			screen:   "● Text\nReading file\nReading something\n❯",
			expected: "Text\nReading something",
		},
		{
			name:     "trailing prompt variants",
			screen:   "● Text\n❯ some text\nmore",
			expected: "Text",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractResponseText(tc.screen)
			if got != tc.expected {
				t.Errorf("extractResponseText() =\n%q\nwant:\n%q", got, tc.expected)
			}
		})
	}
}

func TestStopHookTimeout_Scenarios(t *testing.T) {
	tests := []struct {
		name              string
		timeout           time.Duration
		setupHook         bool
		hookDelay         time.Duration
		screenProgressive bool
		expectError       bool
		errorContains     string
		expectedSource    ResponseSource
		expectedText      string
	}{
		{
			name:          "context cancellation before hook",
			timeout:       100 * time.Millisecond,
			setupHook:     true,
			hookDelay:     200 * time.Millisecond,
			expectError:   true,
			errorContains: "context",
		},
		{
			name:           "hook arrives within timeout",
			timeout:        500 * time.Millisecond,
			setupHook:      true,
			hookDelay:      50 * time.Millisecond,
			expectError:    false,
			expectedSource: ResponseSourceStopHook,
			expectedText:   "hook content",
		},
		{
			name:          "timeout waiting for hook",
			timeout:       50 * time.Millisecond,
			setupHook:     true,
			hookDelay:     200 * time.Millisecond,
			expectError:   true,
			errorContains: "deadline",
		},
		{
			name:              "pty completes before timeout",
			timeout:           500 * time.Millisecond,
			setupHook:         false,
			screenProgressive: true,
			expectError:       false,
			expectedSource:    ResponseSourcePTY,
			expectedText:      "Response\nmore",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupTmuxTest(t)

			// Each subtest gets its own pane name: a delayed hook writer
			// from one subtest must not deposit files that a later subtest
			// observes as its own stop-hook response.
			paneName := fmt.Sprintf("timeout-scenario-%d", i)
			paneTarget := "telegram-bridge:" + paneName
			respFile := bridgeRespFile(paneName)
			readyFile := respFile + ".ready"
			t.Cleanup(func() {
				os.Remove(respFile)
				os.Remove(readyFile)
			})

			// Setup progressive screen if needed
			if tc.screenProgressive {
				// Start with incomplete screen
				mockTmuxCommand(t, "capture-pane", "● Response")
				go func() {
					time.Sleep(30 * time.Millisecond)
					mockTmuxCommand(t, "capture-pane", "● Response\nmore")
					time.Sleep(30 * time.Millisecond)
					mockTmuxCommand(t, "capture-pane", "● Response\nmore\n❯")
				}()
			} else {
				// Start with complete screen to avoid response-start timeout
				mockTmuxCommand(t, "capture-pane", "● Response\n❯")
			}

			// Setup hook if needed. The writer is tracked so the subtest can
			// drain it before finishing — a write landing after cleanup would
			// leave a stray file behind.
			var writers sync.WaitGroup
			if tc.setupHook {
				writers.Add(1)
				go func() {
					defer writers.Done()
					time.Sleep(tc.hookDelay)
					_ = os.WriteFile(respFile, []byte("hook content"), 0600)
					_ = os.WriteFile(readyFile, []byte("ready"), 0600)
				}()
			}

			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()

			text, source, err := NewPTYManager().WaitForResponseWithSource(ctx, paneTarget, "", nil)

			if tc.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if tc.errorContains != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.errorContains)) {
					t.Errorf("expected error containing %q, got %q", tc.errorContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if source != tc.expectedSource {
					t.Errorf("source = %q, want %q", source, tc.expectedSource)
				}
				if text != tc.expectedText {
					t.Errorf("text = %q, want %q", text, tc.expectedText)
				}
			}
			writers.Wait()
		})
	}
}
