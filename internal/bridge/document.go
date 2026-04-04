package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// processDocument downloads a document from the proxy and returns the local path.
// For supported file types, Claude's Read tool can handle them directly.
// For unsupported types, returns an error message to display to the user.
func (m *SessionManager) processDocument(
	ctx context.Context,
	chatID, messageID int64,
	content *contract.Content,
) (docPath string, unsupportedMsg string, cleanupPaths []string, err error) {
	dir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// Extract file extension from original filename or MIME type
	var ext string
	if content.FileName != nil && *content.FileName != "" {
		ext = filepath.Ext(*content.FileName)
		if ext == "" {
			ext = documentExtFromMime(content.MimeType)
		}
	} else {
		ext = documentExtFromMime(content.MimeType)
	}
	if ext == "" {
		ext = ".bin"
	}

	// Use message_id as filename (not the original filename for security)
	docPath = filepath.Join(dir, fmt.Sprintf("%d%s", messageID, ext))

	if err := downloadFile(ctx, m.proxyURL+"/file/"+*content.FileID, docPath); err != nil {
		return "", "", nil, fmt.Errorf("download document: %w", err)
	}
	cleanupPaths = append(cleanupPaths, docPath)

	// Check if the file type is supported
	if !isDocumentSupported(content) {
		unsupportedMsg = getUnsupportedMessage(content)
	}

	return docPath, unsupportedMsg, cleanupPaths, nil
}

// isDocumentSupported returns true if Claude's Read tool can handle this file type.
func isDocumentSupported(content *contract.Content) bool {
	if content.MimeType == nil {
		return false
	}
	mime := *content.MimeType

	// Text and code files
	if strings.HasPrefix(mime, "text/") {
		return true
	}

	// Specific supported MIME types
	switch mime {
	case "application/json", "application/xml", "application/x-yaml", "application/yaml",
		"application/x-sh", "application/x-shellscript", "application/javascript",
		"application/x-javascript", "text/javascript", "application/typescript",
		"application/x-python", "text/x-python", "application/x-perl", "application/x-ruby",
		"application/x-go", "text/x-go", "application/x-rust", "application/x-java",
		"application/x-c", "application/x-c++", "application/x-csharp", "application/x-php",
		"application/x-tcl", "application/x-lua", "application/x-sql", "application/sql",
		"application/x-httpd-php", "application/vnd.php", "application/markdown",
		"text/markdown", "application/x-toml", "application/x-bat", "application/x-powershell",
		"application/x-msdos-program", "application/x-shockwave-flash",
		"application/pdf", "application/x-pdf":
		return true
	}

	// Images
	if strings.HasPrefix(mime, "image/") {
		return true
	}

	// Jupyter notebooks
	if mime == "application/x-ipynb+json" || (content.FileName != nil && strings.HasSuffix(*content.FileName, ".ipynb")) {
		return true
	}

	return false
}

// getUnsupportedMessage returns a user-friendly message for unsupported file types.
func getUnsupportedMessage(content *contract.Content) string {
	var fileName string
	if content.FileName != nil {
		fileName = *content.FileName
	} else {
		fileName = "uploaded file"
	}
	return fmt.Sprintf("⚠️ This file type (%s) is not directly supported. Claude can process text, code, PDF, and image files.", fileName)
}

// documentExtFromMime returns a file extension for a given MIME type.
// Defaults to ".txt" for text types, ".bin" for unknown.
func documentExtFromMime(mime *string) string {
	if mime == nil {
		return ".bin"
	}
	m := *mime

	switch {
	case strings.HasPrefix(m, "text/"):
		return ".txt"
	case m == "application/json":
		return ".json"
	case m == "application/xml":
		return ".xml"
	case m == "application/x-yaml" || m == "application/yaml":
		return ".yaml"
	case m == "application/pdf" || m == "application/x-pdf":
		return ".pdf"
	case m == "application/x-ipynb+json":
		return ".ipynb"
	case strings.HasPrefix(m, "image/"):
		switch m {
		case "image/jpeg":
			return ".jpg"
		case "image/png":
			return ".png"
		case "image/gif":
			return ".gif"
		case "image/webp":
			return ".webp"
		default:
			return ".img"
		}
	case strings.Contains(m, "javascript"):
		return ".js"
	case strings.Contains(m, "typescript"):
		return ".ts"
	case strings.Contains(m, "python"):
		return ".py"
	case strings.Contains(m, "perl"):
		return ".pl"
	case strings.Contains(m, "ruby"):
		return ".rb"
	case strings.Contains(m, "go"):
		return ".go"
	case strings.Contains(m, "rust"):
		return ".rs"
	case strings.Contains(m, "java"):
		return ".java"
	case strings.Contains(m, "c++") || strings.Contains(m, "cplusplus"):
		return ".cpp"
	case strings.Contains(m, "csharp"):
		return ".cs"
	case strings.Contains(m, "php"):
		return ".php"
	case strings.Contains(m, "shell") || strings.Contains(m, "sh"):
		return ".sh"
	case strings.Contains(m, "powershell") || strings.Contains(m, "ps1"):
		return ".ps1"
	case strings.Contains(m, "sql"):
		return ".sql"
	case strings.Contains(m, "markdown"):
		return ".md"
	case strings.Contains(m, "toml"):
		return ".toml"
	default:
		return ".bin"
	}
}
