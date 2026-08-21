package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useDocumentTempDir keeps document attachment tests isolated from the
// production staging directory. processDocument shares imageTempDir with the
// other attachment processors.
func useDocumentTempDir(t *testing.T) string {
	t.Helper()
	original := imageTempDir
	imageTempDir = t.TempDir()
	t.Cleanup(func() { imageTempDir = original })
	return imageTempDir
}

func newDocumentSessionManager(proxyURL string) *SessionManager {
	return &SessionManager{proxyURL: proxyURL}
}

// TestIsDocumentSupported_TextFiles tests that text files are supported.
func TestIsDocumentSupported_TextFiles(t *testing.T) {
	tests := []struct {
		name      string
		mimeType  string
		supported bool
	}{
		// Plain text
		{"text/plain", "text/plain", true},
		{"text/html", "text/html", true},
		{"text/css", "text/css", true},
		{"text/csv", "text/csv", true},

		// Text with specific formats
		{"text/javascript", "text/javascript", true},
		{"text/markdown", "text/markdown", true},
		{"text/x-python", "text/x-python", true},
		{"text/x-go", "text/x-go", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := &contract.Content{
				MimeType: &tc.mimeType,
			}
			got := isDocumentSupported(content)
			if got != tc.supported {
				t.Errorf("isDocumentSupported(%q) = %v, want %v", tc.mimeType, got, tc.supported)
			}
		})
	}
}

// TestIsDocumentSupported_CodeFiles tests that code files are supported.
func TestIsDocumentSupported_CodeFiles(t *testing.T) {
	tests := []struct {
		name      string
		mimeType  string
		supported bool
	}{
		// Data formats
		{"application/json", "application/json", true},
		{"application/xml", "application/xml", true},
		{"application/x-yaml", "application/x-yaml", true},
		{"application/yaml", "application/yaml", true},

		// Shell scripts
		{"application/x-sh", "application/x-sh", true},
		{"application/x-shellscript", "application/x-shellscript", true},

		// JavaScript
		{"application/javascript", "application/javascript", true},
		{"application/x-javascript", "application/x-javascript", true},

		// TypeScript
		{"application/typescript", "application/typescript", true},

		// Programming languages
		{"application/x-python", "application/x-python", true},
		{"application/x-perl", "application/x-perl", true},
		{"application/x-ruby", "application/x-ruby", true},
		{"application/x-go", "application/x-go", true},
		{"application/x-rust", "application/x-rust", true},
		{"application/x-java", "application/x-java", true},
		{"application/x-c", "application/x-c", true},
		{"application/x-c++", "application/x-c++", true},
		{"application/x-csharp", "application/x-csharp", true},
		{"application/x-php", "application/x-php", true},
		{"application/x-tcl", "application/x-tcl", true},
		{"application/x-lua", "application/x-lua", true},
		{"application/x-sql", "application/x-sql", true},
		{"application/sql", "application/sql", true},

		// Web and config
		{"application/x-httpd-php", "application/x-httpd-php", true},
		{"application/vnd.php", "application/vnd.php", true},
		{"application/markdown", "application/markdown", true},
		{"application/x-toml", "application/x-toml", true},
		{"application/x-bat", "application/x-bat", true},
		{"application/x-powershell", "application/x-powershell", true},

		// PDF
		{"application/pdf", "application/pdf", true},
		{"application/x-pdf", "application/x-pdf", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := &contract.Content{
				MimeType: &tc.mimeType,
			}
			got := isDocumentSupported(content)
			if got != tc.supported {
				t.Errorf("isDocumentSupported(%q) = %v, want %v", tc.mimeType, got, tc.supported)
			}
		})
	}
}

// TestIsDocumentSupported_Images tests that images are supported.
func TestIsDocumentSupported_Images(t *testing.T) {
	tests := []struct {
		name      string
		mimeType  string
		supported bool
	}{
		{"image/jpeg", "image/jpeg", true},
		{"image/png", "image/png", true},
		{"image/gif", "image/gif", true},
		{"image/webp", "image/webp", true},
		{"image/svg+xml", "image/svg+xml", true},
		{"image/bmp", "image/bmp", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := &contract.Content{
				MimeType: &tc.mimeType,
			}
			got := isDocumentSupported(content)
			if got != tc.supported {
				t.Errorf("isDocumentSupported(%q) = %v, want %v", tc.mimeType, got, tc.supported)
			}
		})
	}
}

// TestIsDocumentSupported_JupyterNotebooks tests that Jupyter notebooks are supported.
func TestIsDocumentSupported_JupyterNotebooks(t *testing.T) {
	mimeType := "application/x-ipynb+json"
	content := &contract.Content{
		MimeType: &mimeType,
	}

	got := isDocumentSupported(content)
	if !got {
		t.Errorf("isDocumentSupported(%q) = %v, want true", mimeType, got)
	}
}

// TestIsDocumentSupported_JupyterByFilename tests that .ipynb extension is recognized.
func TestIsDocumentSupported_JupyterByFilename(t *testing.T) {
	mimeType := "application/octet-stream"
	fileName := "notebook.ipynb"
	content := &contract.Content{
		MimeType: &mimeType,
		FileName: &fileName,
	}

	got := isDocumentSupported(content)
	if !got {
		t.Errorf("isDocumentSupported(filename=%q) = %v, want true", fileName, got)
	}
}

// TestIsDocumentSupported_UnsupportedTypes tests that unsupported types are rejected.
func TestIsDocumentSupported_UnsupportedTypes(t *testing.T) {
	tests := []struct {
		name      string
		mimeType  string
		supported bool
	}{
		{"application/zip", "application/zip", false},
		{"application/x-rar-compressed", "application/x-rar-compressed", false},
		{"application/x-7z-compressed", "application/x-7z-compressed", false},
		{"application/x-tar", "application/x-tar", false},
		{"video/mp4", "video/mp4", false},
		{"audio/mpeg", "audio/mpeg", false},
		{"application/x-msdownload", "application/x-msdownload", false},
		{"application/x-executable", "application/x-executable", false},
		{"application/x-dosexec", "application/x-dosexec", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := &contract.Content{
				MimeType: &tc.mimeType,
			}
			got := isDocumentSupported(content)
			if got != tc.supported {
				t.Errorf("isDocumentSupported(%q) = %v, want %v", tc.mimeType, got, tc.supported)
			}
		})
	}
}

// TestIsDocumentSupported_NilMime tests that nil MIME type returns false.
func TestIsDocumentSupported_NilMime(t *testing.T) {
	content := &contract.Content{
		MimeType: nil,
	}
	got := isDocumentSupported(content)
	if got {
		t.Errorf("isDocumentSupported(nil MIME) = %v, want false", got)
	}
}

// TestDocumentExtFromMime_Text tests extension mapping for text types.
func TestDocumentExtFromMime_Text(t *testing.T) {
	tests := []struct {
		mime string
		want string
	}{
		{"text/plain", ".txt"},
		{"text/html", ".txt"},
		{"text/css", ".txt"},
		{"text/csv", ".txt"},
	}

	for _, tc := range tests {
		t.Run(tc.mime, func(t *testing.T) {
			got := documentExtFromMime(&tc.mime)
			if got != tc.want {
				t.Errorf("documentExtFromMime(%q) = %q, want %q", tc.mime, got, tc.want)
			}
		})
	}
}

// TestDocumentExtFromMime_Code tests extension mapping for code files.
func TestDocumentExtFromMime_Code(t *testing.T) {
	tests := []struct {
		mime string
		want string
	}{
		{"application/json", ".json"},
		{"application/xml", ".xml"},
		{"application/x-yaml", ".yaml"},
		{"application/yaml", ".yaml"},
		{"application/x-shellscript", ".sh"},
		{"application/x-sh", ".sh"},
		{"application/javascript", ".js"},
		{"application/typescript", ".ts"},
		{"application/x-python", ".py"},
		{"application/x-perl", ".pl"},
		{"application/x-ruby", ".rb"},
		{"application/x-go", ".go"},
		{"application/x-rust", ".rs"},
		{"application/x-java", ".java"},
		{"application/x-c", ".c"},
		{"application/x-c++", ".cpp"},
		{"application/x-csharp", ".cs"},
		{"application/x-php", ".php"},
		{"application/x-sql", ".sql"},
		{"application/sql", ".sql"},
		{"application/markdown", ".md"},
		{"application/x-toml", ".toml"},
		{"application/x-bat", ".bat"},
		{"application/x-powershell", ".ps1"},
	}

	for _, tc := range tests {
		t.Run(tc.mime, func(t *testing.T) {
			got := documentExtFromMime(&tc.mime)
			if got != tc.want {
				t.Errorf("documentExtFromMime(%q) = %q, want %q", tc.mime, got, tc.want)
			}
		})
	}
}

// TestDocumentExtFromMime_Images tests extension mapping for images.
func TestDocumentExtFromMime_Images(t *testing.T) {
	tests := []struct {
		mime string
		want string
	}{
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"image/svg+xml", ".img"},
		{"image/bmp", ".img"},
		{"image/tiff", ".img"},
	}

	for _, tc := range tests {
		t.Run(tc.mime, func(t *testing.T) {
			got := documentExtFromMime(&tc.mime)
			if got != tc.want {
				t.Errorf("documentExtFromMime(%q) = %q, want %q", tc.mime, got, tc.want)
			}
		})
	}
}

// TestDocumentExtFromMime_Other tests extension mapping for other file types.
func TestDocumentExtFromMime_Other(t *testing.T) {
	tests := []struct {
		mime string
		want string
	}{
		{"application/pdf", ".pdf"},
		{"application/x-pdf", ".pdf"},
		{"application/x-ipynb+json", ".ipynb"},
		{"application/zip", ".bin"},
		{"application/octet-stream", ".bin"},
	}

	for _, tc := range tests {
		t.Run(tc.mime, func(t *testing.T) {
			got := documentExtFromMime(&tc.mime)
			if got != tc.want {
				t.Errorf("documentExtFromMime(%q) = %q, want %q", tc.mime, got, tc.want)
			}
		})
	}
}

// TestDocumentExtFromMime_Nil tests that nil MIME type returns .bin.
func TestDocumentExtFromMime_Nil(t *testing.T) {
	got := documentExtFromMime(nil)
	if got != ".bin" {
		t.Errorf("documentExtFromMime(nil) = %q, want .bin", got)
	}
}

// TestDocumentProcessRouting verifies that each document category reaches the
// attachment path Claude can read, while unsupported types are returned with a
// user-facing warning. It also pins the staged filename behavior: an original
// extension is retained, but the user-controlled basename is never used.
func TestDocumentProcessRouting(t *testing.T) {
	tests := []struct {
		name            string
		mimeType        string
		fileName        string
		wantExtension   string
		wantUnsupported bool
	}{
		{
			name:          "PDF routes as a readable attachment",
			mimeType:      "application/pdf",
			fileName:      "quarterly-report.pdf",
			wantExtension: ".pdf",
		},
		{
			name:          "plain text without a filename uses text extension",
			mimeType:      "text/plain",
			wantExtension: ".txt",
		},
		{
			name:          "Go source routes as code",
			mimeType:      "application/x-go",
			fileName:      "main.go",
			wantExtension: ".go",
		},
		{
			name:          "image routes as a readable attachment",
			mimeType:      "image/png",
			fileName:      "diagram.png",
			wantExtension: ".png",
		},
		{
			name:          "Jupyter notebook filename routes as code",
			mimeType:      "application/octet-stream",
			fileName:      "analysis.ipynb",
			wantExtension: ".ipynb",
		},
		{
			name:            "archive returns an unsupported type warning",
			mimeType:        "application/zip",
			fileName:        "release.zip",
			wantExtension:   ".zip",
			wantUnsupported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempRoot := useDocumentTempDir(t)
			const (
				chatID    = int64(12345)
				messageID = int64(67890)
			)
			fileID := strings.ReplaceAll(tt.name, " ", "-")

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/file/"+fileID, r.URL.Path)
				_, _ = w.Write([]byte("document bytes"))
			}))
			t.Cleanup(server.Close)

			content := &contract.Content{
				FileID:   &fileID,
				MimeType: &tt.mimeType,
			}
			if tt.fileName != "" {
				content.FileName = &tt.fileName
			}

			path, unsupportedMsg, cleanupPaths, err := newDocumentSessionManager(server.URL).processDocument(
				context.Background(), chatID, messageID, content,
			)
			require.NoError(t, err)

			wantPath := filepath.Join(tempRoot, fmt.Sprintf("%d", chatID), fmt.Sprintf("%d%s", messageID, tt.wantExtension))
			assert.Equal(t, wantPath, path)
			assert.Equal(t, []string{wantPath}, cleanupPaths)
			assert.FileExists(t, path)
			assert.Equal(t, []byte("document bytes"), mustReadDocument(t, path))

			if tt.wantUnsupported {
				assert.Contains(t, unsupportedMsg, tt.fileName)
				assert.Contains(t, unsupportedMsg, "not directly supported")
			} else {
				assert.Empty(t, unsupportedMsg)
			}
		})
	}
}

// TestDocumentProcessSizeBounds confirms that a document at the proxy's 20 MiB
// limit is staged, whereas the proxy's over-limit response is surfaced and no
// partial attachment is returned to the caller.
func TestDocumentProcessSizeBounds(t *testing.T) {
	tests := []struct {
		name      string
		fileSize  int64
		status    int
		wantErr   string
		wantPaths bool
	}{
		{
			name:      "at proxy limit",
			fileSize:  telegram.MaxFileSize,
			status:    http.StatusOK,
			wantPaths: true,
		},
		{
			name:     "one byte over proxy limit",
			fileSize: telegram.MaxFileSize + 1,
			status:   http.StatusRequestEntityTooLarge,
			wantErr:  "status 413",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useDocumentTempDir(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				w.WriteHeader(tt.status)
				if tt.status == http.StatusOK {
					_, _ = w.Write([]byte("within limit"))
				}
			}))
			t.Cleanup(server.Close)

			fileID := "sized-document"
			mimeType := "application/pdf"
			path, unsupportedMsg, cleanupPaths, err := newDocumentSessionManager(server.URL).processDocument(
				context.Background(), 1, 2, &contract.Content{
					FileID:   &fileID,
					FileSize: &tt.fileSize,
					MimeType: &mimeType,
				},
			)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "download document")
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Empty(t, path)
				assert.Empty(t, unsupportedMsg)
				assert.Empty(t, cleanupPaths)
				return
			}

			require.NoError(t, err)
			assert.NotEmpty(t, path)
			assert.Empty(t, unsupportedMsg)
			if tt.wantPaths {
				assert.Equal(t, []string{path}, cleanupPaths)
			}
		})
	}
}

// TestDocumentProcessErrorPaths covers failures before and during the
// download. Unsupported document MIME types are intentionally not an error:
// they are handled by TestDocumentProcessRouting as a warning after download.
func TestDocumentProcessErrorPaths(t *testing.T) {
	t.Run("staging directory creation fails", func(t *testing.T) {
		original := imageTempDir
		imageTempDir = filepath.Join(t.TempDir(), "not-a-directory")
		require.NoError(t, os.WriteFile(imageTempDir, []byte("x"), 0o644))
		t.Cleanup(func() { imageTempDir = original })

		fileID := "unreachable"
		mimeType := "text/plain"
		path, unsupportedMsg, cleanupPaths, err := newDocumentSessionManager("http://127.0.0.1:1").processDocument(
			context.Background(), 1, 2, &contract.Content{FileID: &fileID, MimeType: &mimeType},
		)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "mkdir")
		assert.Empty(t, path)
		assert.Empty(t, unsupportedMsg)
		assert.Empty(t, cleanupPaths)
	})

	t.Run("proxy download failure", func(t *testing.T) {
		useDocumentTempDir(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		t.Cleanup(server.Close)

		fileID := "expired-document"
		mimeType := "text/plain"
		path, unsupportedMsg, cleanupPaths, err := newDocumentSessionManager(server.URL).processDocument(
			context.Background(), 1, 2, &contract.Content{FileID: &fileID, MimeType: &mimeType},
		)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "download document")
		assert.Contains(t, err.Error(), "status 502")
		assert.Empty(t, path)
		assert.Empty(t, unsupportedMsg)
		assert.Empty(t, cleanupPaths)
	})
}

func mustReadDocument(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// TestGetUnsupportedMessage tests the unsupported message generation.
func TestGetUnsupportedMessage(t *testing.T) {
	tests := []struct {
		name         string
		fileName     string
		containsText string
	}{
		{
			name:         "with filename",
			fileName:     "archive.zip",
			containsText: "archive.zip",
		},
		{
			name:         "without filename",
			fileName:     "",
			containsText: "uploaded file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content := &contract.Content{}
			if tc.fileName != "" {
				content.FileName = &tc.fileName
			}
			got := getUnsupportedMessage(content)

			// Check that the message contains expected text
			if !contains(got, tc.containsText) {
				t.Errorf("getUnsupportedMessage() = %q, should contain %q", got, tc.containsText)
			}

			// Check that the message contains warning emoji
			if !contains(got, "⚠️") {
				t.Errorf("getUnsupportedMessage() = %q, should contain warning emoji", got)
			}

			// Check that the message mentions supported types
			if !contains(got, "text") && !contains(got, "code") {
				t.Errorf("getUnsupportedMessage() = %q, should mention supported types", got)
			}
		})
	}
}

// contains is a helper to check if a string contains a substring.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && findInString(s, substr))
}

func findInString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
