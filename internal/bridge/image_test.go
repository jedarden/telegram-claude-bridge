package bridge

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Test Helpers ─────────────────────────────────────────────────────────────

// useImageTempDir points imageTempDir at a fresh temp directory for the
// duration of the test, restoring the original value afterwards. Image tests
// must never touch the real /tmp/telegram-bridge.
func useImageTempDir(t *testing.T) string {
	t.Helper()
	original := imageTempDir
	imageTempDir = t.TempDir()
	t.Cleanup(func() { imageTempDir = original })
	return imageTempDir
}

// newImageSessionManager returns a SessionManager wired for image tests:
// processPhoto only touches proxyURL, so no db, sender, or commandExec is needed.
func newImageSessionManager(proxyURL string) *SessionManager {
	return &SessionManager{
		proxyURL: proxyURL,
	}
}

// createTestJPEG creates a minimal valid JPEG image with the given dimensions.
func createTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Fill with a gradient pattern for visual distinctiveness
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x * 255) / w),
				G: uint8((y * 255) / h),
				B: 128,
				A: 255,
			})
		}
	}
	err := jpeg.Encode(buf, img, &jpeg.Options{Quality: 90})
	require.NoError(t, err, "failed to create test JPEG")
	return buf.Bytes()
}

// createTestPNG creates a minimal valid PNG image with the given dimensions.
func createTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// Fill with a different pattern
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: 128,
				G: uint8((x * 255) / w),
				B: uint8((y * 255) / h),
				A: 255,
			})
		}
	}
	err := png.Encode(buf, img)
	require.NoError(t, err, "failed to create test PNG")
	return buf.Bytes()
}

// createInvalidImageData creates data that is not a valid image.
func createInvalidImageData(t *testing.T) []byte {
	t.Helper()
	return []byte("This is not an image file, just plain text.")
}

// ── Image Type Detection ──────────────────────────────────────────────────────

// TestImageTypeDetection verifies that resizePhotoFile can handle both JPEG and PNG
// input formats. Note: Images are only re-encoded to JPEG when they exceed the size limit
// and need resizing. Small images within bounds retain their original format.
func TestImageTypeDetection(t *testing.T) {
	tests := []struct {
		name           string
		imageData      []byte
		description    string
		expectJPEG     bool   // Whether output should be JPEG (resized) or original format
		expectedFormat string // Expected format after processing
	}{
		{
			name:           "JPEG input - within bounds, stays JPEG",
			imageData:      createTestJPEG(t, 400, 300),
			description:    "JPEG within bounds should not be modified",
			expectJPEG:     true,
			expectedFormat: "jpeg",
		},
		{
			name:           "PNG input - within bounds, stays PNG",
			imageData:      createTestPNG(t, 400, 300),
			description:    "PNG within bounds stays PNG (no resize = no re-encode)",
			expectJPEG:     false,
			expectedFormat: "png",
		},
		{
			name:           "large JPEG resized, stays JPEG",
			imageData:      createTestJPEG(t, 1200, 900),
			description:    "Large JPEG should be resized and remain JPEG",
			expectJPEG:     true,
			expectedFormat: "jpeg",
		},
		{
			name:           "large PNG resized, converted to JPEG",
			imageData:      createTestPNG(t, 900, 1200),
			description:    "Large PNG should be resized and re-encoded as JPEG",
			expectJPEG:     true,
			expectedFormat: "jpeg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a temporary file with the test image data
			tmpFile := filepath.Join(t.TempDir(), "test_image.jpg")
			err := os.WriteFile(tmpFile, tt.imageData, 0o644)
			require.NoError(t, err, "failed to write test image file")

			// Attempt to resize the file
			err = resizePhotoFile(tmpFile)

			// Should succeed for both JPEG and PNG inputs
			assert.NoError(t, err, tt.description)

			// Verify the file still exists and is a valid image
			data, err := os.ReadFile(tmpFile)
			require.NoError(t, err, "failed to read resized file")
			require.True(t, len(data) >= 2, "file should have at least 2 bytes")

			// Check the format based on expectation
			if tt.expectJPEG {
				// Should be JPEG (JPEG files start with 0xFF 0xD8)
				assert.Equal(t, []byte{0xFF, 0xD8}, data[:2], "output should be JPEG format")
			} else {
				// Should be PNG (PNG files start with 0x89 0x50 0x4E 0x47 0x0D 0x0A 0x1A 0x0A)
				assert.Equal(t, []byte{0x89, 0x50}, data[:2], "output should be PNG format")
			}

			// Verify it can be decoded as an image
			cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
			assert.NoError(t, err, "output should be decodable")
			assert.Equal(t, tt.expectedFormat, format, "output format should match expectation")

			// Verify dimensions are within bounds (800px max)
			assert.LessOrEqual(t, cfg.Width, imageMaxDim, "width should be within max dimension")
			assert.LessOrEqual(t, cfg.Height, imageMaxDim, "height should be within max dimension")
		})
	}
}

// ── Size Limit Validation ─────────────────────────────────────────────────────

// TestImageSizeLimitValidation verifies that images larger than imageMaxDim
// are resized down, while images within the limit are left unchanged.
func TestImageSizeLimitValidation(t *testing.T) {
	tests := []struct {
		name           string
		inputW, inputH int
		shouldResize   bool
	}{
		{
			name:         "100x100 - no resize needed",
			inputW:       100,
			inputH:       100,
			shouldResize: false,
		},
		{
			name:         "800x600 - at boundary, no resize",
			inputW:       800,
			inputH:       600,
			shouldResize: false,
		},
		{
			name:         "600x800 - at boundary, no resize",
			inputW:       600,
			inputH:       800,
			shouldResize: false,
		},
		{
			name:         "801x600 - exceeds limit, should resize",
			inputW:       801,
			inputH:       600,
			shouldResize: true,
		},
		{
			name:         "600x801 - exceeds limit, should resize",
			inputW:       600,
			inputH:       801,
			shouldResize: true,
		},
		{
			name:         "1600x1200 - exceeds both dimensions",
			inputW:       1600,
			inputH:       1200,
			shouldResize: true,
		},
		{
			name:         "2000x500 - panoramic, should resize",
			inputW:       2000,
			inputH:       500,
			shouldResize: true,
		},
		{
			name:         "500x2000 - tall, should resize",
			inputW:       500,
			inputH:       2000,
			shouldResize: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test image with specified dimensions
			imageData := createTestJPEG(t, tt.inputW, tt.inputH)
			tmpFile := filepath.Join(t.TempDir(), "test.jpg")

			// Write the original image
			err := os.WriteFile(tmpFile, imageData, 0o644)
			require.NoError(t, err, "failed to write test image")

			// Resize the file
			err = resizePhotoFile(tmpFile)
			require.NoError(t, err, "resize should succeed")

			// Read back and verify dimensions
			data, err := os.ReadFile(tmpFile)
			require.NoError(t, err, "failed to read resized file")

			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			require.NoError(t, err, "failed to decode resized image")

			// Verify output dimensions are within bounds
			assert.LessOrEqual(t, cfg.Width, imageMaxDim, "output width should be <= %d", imageMaxDim)
			assert.LessOrEqual(t, cfg.Height, imageMaxDim, "output height should be <= %d", imageMaxDim)

			if tt.shouldResize {
				// File should have been modified (size likely changed)
				// Note: This is a weak check - JPEG re-encoding might result in similar size
				// but the dimensions should definitely be different if resized
				if tt.inputW > imageMaxDim || tt.inputH > imageMaxDim {
					// At least one dimension should be smaller
					maxInputDim := tt.inputW
					if tt.inputH > maxInputDim {
						maxInputDim = tt.inputH
					}
					assert.LessOrEqual(t, max(cfg.Width, cfg.Height), maxInputDim,
						"max dimension should be smaller or equal after resize")
				}
			} else {
				// Within limit - dimensions should be unchanged
				assert.Equal(t, tt.inputW, cfg.Width, "width should be unchanged when within limit")
				assert.Equal(t, tt.inputH, cfg.Height, "height should be unchanged when within limit")
			}
		})
	}
}

// TestImageSizeExactBounds tests the exact boundary conditions for image dimensions.
func TestImageSizeExactBounds(t *testing.T) {
	tests := []struct {
		name         string
		w, h         int
		expectsSkip  bool
	}{
		{
			name:        "799x799 - just under limit",
			w:           799,
			h:           799,
			expectsSkip: true, // No resize needed
		},
		{
			name:        "800x800 - exactly at limit",
			w:           800,
			h:           800,
			expectsSkip: true, // No resize needed
		},
		{
			name:        "801x801 - just over limit",
			w:           801,
			h:           801,
			expectsSkip: false, // Resize needed
		},
		{
			name:        "800x1 - at limit width, minimal height",
			w:           800,
			h:           1,
			expectsSkip: true, // No resize needed
		},
		{
			name:        "1x800 - minimal width, at limit height",
			w:           1,
			h:           800,
			expectsSkip: true, // No resize needed
		},
		{
			name:        "801x1 - just over limit width",
			w:           801,
			h:           1,
			expectsSkip: false, // Resize needed
		},
		{
			name:        "1x801 - just over limit height",
			w:           1,
			h:           801,
			expectsSkip: false, // Resize needed
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imageData := createTestJPEG(t, tt.w, tt.h)
			tmpFile := filepath.Join(t.TempDir(), "test.jpg")

			err := os.WriteFile(tmpFile, imageData, 0o644)
			require.NoError(t, err)

			err = resizePhotoFile(tmpFile)
			require.NoError(t, err, "resize should succeed")

			data, err := os.ReadFile(tmpFile)
			require.NoError(t, err, "failed to read resized file")

			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			require.NoError(t, err, "failed to decode resized image")

			// Always verify output is within bounds
			assert.LessOrEqual(t, cfg.Width, imageMaxDim, "output width must be <= %d", imageMaxDim)
			assert.LessOrEqual(t, cfg.Height, imageMaxDim, "output height must be <= %d", imageMaxDim)

			if tt.expectsSkip {
				// Dimensions should remain unchanged when within/at limit
				assert.Equal(t, tt.w, cfg.Width, "width should be unchanged")
				assert.Equal(t, tt.h, cfg.Height, "height should be unchanged")
			} else {
				// At least one dimension should be reduced
				maxOriginal := tt.w
				if tt.h > maxOriginal {
					maxOriginal = tt.h
				}
				maxResized := cfg.Width
				if cfg.Height > maxResized {
					maxResized = cfg.Height
				}
				assert.LessOrEqual(t, maxResized, maxOriginal, "max dimension should not increase")
				assert.LessOrEqual(t, maxResized, imageMaxDim, "max dimension should be within limit")
			}
		})
	}
}

// ── Error Paths for Unsupported Formats ────────────────────────────────────────

// TestImageUnsupportedFormats tests error handling for unsupported image formats.
func TestImageUnsupportedFormats(t *testing.T) {
	tests := []struct {
		name        string
		imageData   []byte
		description string
	}{
		{
			name:        "plain text file rejected",
			imageData:   createInvalidImageData(t),
			description: "Non-image data should fail decode",
		},
		{
			name:        "empty file rejected",
			imageData:   []byte{},
			description: "Empty file should fail decode",
		},
		{
			name:        "partial JPEG header rejected",
			imageData:   []byte{0xFF, 0xD8}, // Incomplete JPEG
			description: "Truncated image should fail decode",
		},
		{
			name:        "corrupted JPEG data rejected",
			imageData:   append([]byte{0xFF, 0xD8}, []byte("corrupted data")...),
			description: "Corrupted JPEG should fail decode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile := filepath.Join(t.TempDir(), "invalid.jpg")
			err := os.WriteFile(tmpFile, tt.imageData, 0o644)
			require.NoError(t, err, "failed to write test file")

			err = resizePhotoFile(tmpFile)
			assert.Error(t, err, tt.description+" should return error")
			assert.Contains(t, err.Error(), "decode", "error should mention decode failure")
		})
	}
}

// TestImageFileAccessErrors tests error handling for file system access issues.
func TestImageFileAccessErrors(t *testing.T) {
	t.Run("non-existent file returns error", func(t *testing.T) {
		nonExistent := filepath.Join(t.TempDir(), "does_not_exist.jpg")
		err := resizePhotoFile(nonExistent)
		assert.Error(t, err, "opening non-existent file should fail")
		assert.Contains(t, err.Error(), "open", "error should mention open")
	})

	t.Run("directory instead of file returns error", func(t *testing.T) {
		dirPath := t.TempDir()
		err := resizePhotoFile(dirPath)
		assert.Error(t, err, "opening directory should fail")
	})
}

// ── processPhoto Integration Tests ─────────────────────────────────────────────

// TestProcessPhoto_Success tests the full processPhoto flow with mocked downloads.
func TestProcessPhoto_Success(t *testing.T) {
	tests := []struct {
		name         string
		chatID       int64
		messageID    int64
		imageData    []byte
		outputFormat string
		description  string
	}{
		{
			name:         "JPEG photo downloaded and processed",
			chatID:       12345,
			messageID:    67890,
			imageData:    createTestJPEG(t, 400, 300),
			outputFormat: "jpeg",
			description:  "JPEG should be downloaded, saved, and optionally resized",
		},
		{
			name:         "PNG photo downloaded and processed",
			chatID:       98765,
			messageID:    43210,
			imageData:    createTestPNG(t, 400, 300),
			outputFormat: "png",
			description:  "In-bounds PNG should be downloaded without re-encoding",
		},
		{
			name:         "large JPEG resized during processing",
			chatID:       11111,
			messageID:    22222,
			imageData:    createTestJPEG(t, 1600, 1200),
			outputFormat: "jpeg",
			description:  "Large JPEG should be downloaded and resized",
		},
		{
			name:         "small JPEG not resized",
			chatID:       55555,
			messageID:    66666,
			imageData:    createTestJPEG(t, 200, 150),
			outputFormat: "jpeg",
			description:  "Small JPEG should be downloaded but not resized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			tempRoot := useImageTempDir(t)

			// Create mock HTTP server for file downloads
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/file/") {
					w.Header().Set("Content-Type", "image/jpeg")
					w.WriteHeader(http.StatusOK)
					w.Write(tt.imageData)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()

			sm := newImageSessionManager(server.URL)
			fileID := fmt.Sprintf("photo_%d", tt.messageID)

			// Process the photo
			resultPath, err := sm.processPhoto(ctx, tt.chatID, tt.messageID, fileID)
			require.NoError(t, err, tt.description+" should succeed")
			assert.NotEmpty(t, resultPath, "should return a valid file path")

			// Verify the file exists
			_, err = os.Stat(resultPath)
			assert.NoError(t, err, "output file should exist")

			// Verify the file path structure
			expectedDir := filepath.Join(tempRoot, fmt.Sprintf("%d", tt.chatID))
			expectedPath := filepath.Join(expectedDir, fmt.Sprintf("%d.jpg", tt.messageID))
			assert.Equal(t, expectedPath, resultPath, "output path should match expected structure")

			// Files within the size limit are preserved; resized files are JPEG.
			data, err := os.ReadFile(resultPath)
			require.NoError(t, err, "should be able to read output file")

			// Verify dimensions are within bounds
			cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
			require.NoError(t, err, "output should be decodable")
			assert.Equal(t, tt.outputFormat, format, "output format should match resize behavior")
			assert.LessOrEqual(t, cfg.Width, imageMaxDim, "width should be within bounds")
			assert.LessOrEqual(t, cfg.Height, imageMaxDim, "height should be within bounds")

			// Clean up the file as the caller would
			os.Remove(resultPath)
		})
	}
}

// TestProcessPhoto_ErrorPaths tests error handling in processPhoto.
func TestProcessPhoto_ErrorPaths(t *testing.T) {
	const (
		chatID    = int64(12345)
		messageID = int64(67890)
		fileID    = "photo_error_test"
	)

	t.Run("mkdir failure", func(t *testing.T) {
		// Point imageTempDir at a regular file so MkdirAll fails (ENOTDIR)
		original := imageTempDir
		imageTempDir = filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(imageTempDir, []byte("x"), 0o644), "create blocking file")
		t.Cleanup(func() { imageTempDir = original })

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		sm := newImageSessionManager(server.URL)

		resultPath, err := sm.processPhoto(context.Background(), chatID, messageID, fileID)
		assert.Error(t, err, "mkdir failure should fail")
		assert.Contains(t, err.Error(), "mkdir", "error should mention mkdir")
		assert.Empty(t, resultPath, "no path on error")
	})

	t.Run("download failure", func(t *testing.T) {
		useImageTempDir(t)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		sm := newImageSessionManager(server.URL)

		resultPath, err := sm.processPhoto(context.Background(), chatID, messageID, fileID)
		assert.Error(t, err, "download failure should fail")
		assert.Contains(t, err.Error(), "status 500", "error should preserve the download status")
		assert.Empty(t, resultPath, "no path on error")
	})

	t.Run("invalid image data - resize non-fatal", func(t *testing.T) {
		useImageTempDir(t)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/file/") {
				w.Header().Set("Content-Type", "image/jpeg")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("not a valid image"))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		sm := newImageSessionManager(server.URL)

		resultPath, err := sm.processPhoto(context.Background(), chatID, messageID, fileID)
		// processPhoto succeeds even if resize fails - it logs the error and continues
		// The original file is still saved, just not resized
		assert.NoError(t, err, "processPhoto should succeed even with invalid image (resize is non-fatal)")
		assert.NotEmpty(t, resultPath, "should return path even if resize failed")

		// File should exist (was downloaded)
		_, err = os.Stat(resultPath)
		assert.NoError(t, err, "downloaded file should exist even if decode failed")

		// But the file contains invalid data
		os.Remove(resultPath) // Clean up
	})
}

// TestProcessPhoto_IDVariations tests processPhoto with various chat and message ID values.
func TestProcessPhoto_IDVariations(t *testing.T) {
	tests := []struct {
		name      string
		chatID    int64
		messageID int64
	}{
		{name: "minimal IDs", chatID: 0, messageID: 1},
		{name: "large chat ID", chatID: 9999999999999, messageID: 1},
		{name: "large message ID", chatID: 1, messageID: 9999999999999},
		{name: "negative chat ID", chatID: -12345, messageID: 100},
		{name: "special characters in path", chatID: 54321, messageID: 777},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			tempRoot := useImageTempDir(t)

			// For special characters test, inject them into the temp dir path
			if tt.name == "special characters in path" {
				original := imageTempDir
				imageTempDir = filepath.Join(t.TempDir(), "special @dir-[chars] (1)")
				t.Cleanup(func() { imageTempDir = original })
				tempRoot = imageTempDir
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/file/") {
					w.Header().Set("Content-Type", "image/jpeg")
					w.WriteHeader(http.StatusOK)
					w.Write(createTestJPEG(t, 400, 300))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()

			sm := newImageSessionManager(server.URL)
			fileID := fmt.Sprintf("photo_%d", tt.messageID)

			resultPath, err := sm.processPhoto(ctx, tt.chatID, tt.messageID, fileID)
			require.NoError(t, err, "processPhoto should succeed")

			// Verify path structure
			expectedDir := filepath.Join(tempRoot, fmt.Sprintf("%d", tt.chatID))
			expectedPath := filepath.Join(expectedDir, fmt.Sprintf("%d.jpg", tt.messageID))
			assert.Equal(t, expectedPath, resultPath, "path should match expected structure")

			// Verify file exists and is valid
			data, err := os.ReadFile(resultPath)
			require.NoError(t, err, "should be able to read output")
			require.True(t, len(data) >= 2, "file should have JPEG header")
			assert.Equal(t, []byte{0xFF, 0xD8}, data[:2], "should be JPEG format")

			os.Remove(resultPath) // Clean up
		})
	}
}

// ── downloadFile Tests ─────────────────────────────────────────────────────────

// TestDownloadFile tests the generic file download function used by processPhoto.
func TestDownloadFile(t *testing.T) {
	tests := []struct {
		name           string
		responseStatus int
		responseData   []byte
		expectError    bool
		errorContains  string
	}{
		{
			name:           "successful download",
			responseStatus: http.StatusOK,
			responseData:   []byte("test data"),
			expectError:    false,
		},
		{
			name:           "404 not found",
			responseStatus: http.StatusNotFound,
			responseData:   []byte("not found"),
			expectError:    true,
			errorContains:  "status 404",
		},
		{
			name:           "500 internal server error",
			responseStatus: http.StatusInternalServerError,
			responseData:   []byte("server error"),
			expectError:    true,
			errorContains:  "status 500",
		},
		{
			name:           "empty response",
			responseStatus: http.StatusOK,
			responseData:   []byte{},
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			tmpFile := filepath.Join(t.TempDir(), "downloaded_file")

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.responseStatus)
				w.Write(tt.responseData)
			}))
			defer server.Close()

			err := downloadFile(ctx, server.URL, tmpFile)

			if tt.expectError {
				assert.Error(t, err, "download should fail")
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains, "error should contain expected text")
				}
				// Verify no partial file was created
				_, statErr := os.Stat(tmpFile)
				assert.Error(t, statErr, "no file should be created on download error")
			} else {
				assert.NoError(t, err, "download should succeed")
				// Verify file content matches
				data, err := os.ReadFile(tmpFile)
				require.NoError(t, err, "should be able to read downloaded file")
				assert.Equal(t, tt.responseData, data, "downloaded content should match")
			}
		})
	}
}

// TestDownloadFile_ContextCancellation tests that download respects context cancellation.
func TestDownloadFile_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tmpFile := filepath.Join(t.TempDir(), "cancelled_download")

	// Create a server that delays response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cancel context before responding
		cancel()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data"))
	}))
	defer server.Close()

	// This should complete quickly (context already cancelled or about to be)
	err := downloadFile(ctx, server.URL, tmpFile)
	// The behavior depends on timing - either it succeeds or is cancelled
	// Just verify the function doesn't hang and handles the context gracefully
	if err != nil {
		assert.Contains(t, err.Error(), "context", "error should mention context if failed")
	}
}

// ── JPEG Quality Tests ─────────────────────────────────────────────────────────

// TestJPEGQuality verifies that resized images use the correct JPEG quality setting.
func TestJPEGQuality(t *testing.T) {
	t.Run("quality constant is defined", func(t *testing.T) {
		assert.Equal(t, 85, imageJPEGQuality, "JPEG quality should be 85")
	})

	t.Run("resized image uses quality setting", func(t *testing.T) {
		// Create a large image that will be resized
		imageData := createTestJPEG(t, 1600, 1200)
		tmpFile := filepath.Join(t.TempDir(), "quality_test.jpg")

		err := os.WriteFile(tmpFile, imageData, 0o644)
		require.NoError(t, err, "failed to write test image")

		err = resizePhotoFile(tmpFile)
		require.NoError(t, err, "resize should succeed")

		// Read the resized image and verify it's valid JPEG
		data, err := os.ReadFile(tmpFile)
		require.NoError(t, err, "failed to read resized file")

		// Verify JPEG header
		require.True(t, len(data) >= 2, "file should have at least 2 bytes")
		assert.Equal(t, []byte{0xFF, 0xD8}, data[:2], "should be JPEG format")

		// Verify it can be decoded
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		assert.NoError(t, err, "should be decodable")
		assert.Equal(t, "jpeg", format, "should be JPEG format")
		assert.Equal(t, 800, cfg.Width, "width should be resized to 800")
		assert.LessOrEqual(t, cfg.Height, 600, "height should be resized proportionally")
	})
}
