package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
		paneName  string
		expected string
	}{
		{"t100-10", "/tmp/telegram-bridge-resp/t100-10.resp"},
		{"simple", "/tmp/telegram-bridge-resp/simple.resp"},
		{"pane-with-dashes", "/tmp/telegram-bridge-resp/pane-with-dashes.resp"},
	}

	for _, tc := range tests {
		t.Run(tc.paneName, func(t *testing.T) {
			got := bridgeRespFile(tc.paneName)
			if !strings.HasSuffix(got, tc.expected) {
				t.Errorf("bridgeRespFile(%q) = %q, want ending with %q", tc.paneName, got, tc.expected)
			}
		})
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
		{"* Process started", true},
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

		// Invalid - empty parentheses
		{"Bash()", false},

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
			expected: "Text\nmore",
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
		setupContent string
		expectedText string
		expectedOK   bool
	}{
		{
			name:         "ready file exists with content",
			setupReady:   true,
			setupContent: "response text",
			expectedText: "response text",
			expectedOK:   true,
		},
		{
			name:         "ready file exists with multiline",
			setupReady:   true,
			setupContent: "line 1\nline 2\nline 3",
			expectedText: "line 1\nline 2\nline 3",
			expectedOK:   true,
		},
		{
			name:         "ready file exists empty",
			setupReady:   true,
			setupContent: "",
			expectedText: "",
			expectedOK:   true,
		},
		{
			name:         "ready file missing",
			setupReady:   false,
			expectedText: "",
			expectedOK:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			respFile := filepath.Join(tempDir, "test.resp")
			readyFile := respFile + ".ready"

			if tc.setupReady {
				if err := os.WriteFile(respFile, []byte(tc.setupContent), 0600); err != nil {
					t.Fatalf("write resp file: %v", err)
				}
				if err := os.WriteFile(readyFile, []byte("ready"), 0600); err != nil {
					t.Fatalf("write ready file: %v", err)
				}
			}

			text, ok := readStopHookResponse(respFile, readyFile)
			if text != tc.expectedText {
				t.Errorf("text = %q, want %q", text, tc.expectedText)
			}
			if ok != tc.expectedOK {
				t.Errorf("ok = %v, want %v", ok, tc.expectedOK)
			}

			// Files should be cleaned up after reading
			if tc.setupReady {
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

// ── PTYManager Tests ───────────────────────────────────────────────────────────────

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
