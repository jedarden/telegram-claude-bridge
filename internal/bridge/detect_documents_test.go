package bridge

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDetectGeneratedMedia_Documents verifies that document files written after
// the invocation start time are reported in out.DocumentFiles.
func TestDetectGeneratedMedia_Documents(t *testing.T) {
	cwd := t.TempDir()

	// Pre-existing document that must NOT be reported: its modTime is older
	// than startTime.
	stale := filepath.Join(cwd, "old.pdf")
	require.NoError(t, os.WriteFile(stale, []byte("old"), 0o644))
	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(stale, past, past))

	startTime := time.Now().Add(-time.Minute)

	// Files "generated" by the Claude invocation.
	report := filepath.Join(cwd, "report.pdf")
	data := filepath.Join(cwd, "data.csv")
	binary := filepath.Join(cwd, "tool.bin") // unknown extension, ignored
	for _, p := range []string{report, data, binary} {
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	}

	m := &SessionManager{}
	out := &claudeOutput{}
	require.NoError(t, m.detectGeneratedMedia(cwd, startTime, out))

	found := make(map[string]docAttachment, len(out.DocumentFiles))
	for _, doc := range out.DocumentFiles {
		found[doc.Filename] = doc
	}

	require.Contains(t, found, "report.pdf", "expected generated .pdf to be detected")
	require.Contains(t, found, "data.csv", "expected generated .csv to be detected")
	require.Equal(t, report, found["report.pdf"].Path)
	require.Equal(t, data, found["data.csv"].Path)

	require.NotContains(t, found, "old.pdf", "documents older than startTime must be skipped")
	require.NotContains(t, found, "tool.bin", "unknown extensions must be skipped")

	// Documents must not leak into the typed media buckets.
	require.Empty(t, out.AudioFiles)
	require.Empty(t, out.VideoFiles)
	require.Empty(t, out.ImageFiles)
}

// TestDetectGeneratedMedia_DocumentsNoDuplicates verifies repeated scans do not
// append the same document twice.
func TestDetectGeneratedMedia_DocumentsNoDuplicates(t *testing.T) {
	cwd := t.TempDir()
	startTime := time.Now().Add(-time.Minute)

	require.NoError(t, os.WriteFile(filepath.Join(cwd, "summary.pdf"), []byte("x"), 0o644))

	m := &SessionManager{}
	out := &claudeOutput{}
	require.NoError(t, m.detectGeneratedMedia(cwd, startTime, out))
	require.NoError(t, m.detectGeneratedMedia(cwd, startTime, out))

	require.Len(t, out.DocumentFiles, 1)
}
