package bridge

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDetectGeneratedMedia_Images verifies that image files written after
// the invocation start time are reported in out.ImageFiles with the expected
// path and filename.
func TestDetectGeneratedMedia_Images(t *testing.T) {
	cwd := t.TempDir()

	// Pre-existing image that must NOT be reported: its modTime is older
	// than startTime.
	stale := filepath.Join(cwd, "old.png")
	require.NoError(t, os.WriteFile(stale, []byte("old"), 0o644))
	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(stale, past, past))

	startTime := time.Now().Add(-time.Minute)

	// Images "generated" by the Claude invocation — one per recognized
	// extension, plus an uppercase extension to confirm case-insensitive
	// matching.
	generated := []string{
		"plot.png",
		"photo.jpg",
		"photo.jpeg",
		"anim.gif",
		"chart.webp",
		"diagram.svg",
		"SCREENSHOT.PNG",
	}
	for _, name := range generated {
		require.NoError(t, os.WriteFile(filepath.Join(cwd, name), []byte("x"), 0o644))
	}

	// Non-image outputs that must not land in the image bucket.
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "tool.bin"), []byte("x"), 0o644))

	m := &SessionManager{}
	out := &claudeOutput{}
	require.NoError(t, m.detectGeneratedMedia(cwd, startTime, out))

	found := make(map[string]imageAttachment, len(out.ImageFiles))
	for _, img := range out.ImageFiles {
		found[img.Filename] = img
	}

	for _, name := range generated {
		require.Contains(t, found, name, "expected generated image %q to be detected", name)
		require.Equal(t, filepath.Join(cwd, name), found[name].Path)
	}

	require.NotContains(t, found, "old.png", "images older than startTime must be skipped")
	require.NotContains(t, found, "tool.bin", "unknown extensions must be skipped")

	// Images must not leak into the other media buckets.
	require.Empty(t, out.AudioFiles)
	require.Empty(t, out.VideoFiles)
	require.Empty(t, out.DocumentFiles)
}

// TestDetectGeneratedMedia_ImagesSkipHiddenAndTemp verifies that images inside
// hidden directories and temp-prefixed files are not reported.
func TestDetectGeneratedMedia_ImagesSkipHiddenAndTemp(t *testing.T) {
	cwd := t.TempDir()
	startTime := time.Now().Add(-time.Minute)

	hiddenDir := filepath.Join(cwd, ".cache")
	require.NoError(t, os.Mkdir(hiddenDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hiddenDir, "sparkline.png"), []byte("x"), 0o644))

	for _, name := range []string{"tmp_plot.png", "temp_chart.jpg"} {
		require.NoError(t, os.WriteFile(filepath.Join(cwd, name), []byte("x"), 0o644))
	}

	m := &SessionManager{}
	out := &claudeOutput{}
	require.NoError(t, m.detectGeneratedMedia(cwd, startTime, out))

	require.Empty(t, out.ImageFiles, "hidden-dir and temp-prefixed images must be skipped")
}

// TestDetectGeneratedMedia_ImagesNoDuplicates verifies repeated scans do not
// append the same image twice.
func TestDetectGeneratedMedia_ImagesNoDuplicates(t *testing.T) {
	cwd := t.TempDir()
	startTime := time.Now().Add(-time.Minute)

	require.NoError(t, os.WriteFile(filepath.Join(cwd, "render.png"), []byte("x"), 0o644))

	m := &SessionManager{}
	out := &claudeOutput{}
	require.NoError(t, m.detectGeneratedMedia(cwd, startTime, out))
	require.NoError(t, m.detectGeneratedMedia(cwd, startTime, out))

	require.Len(t, out.ImageFiles, 1)
}
