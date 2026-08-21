package bridge

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // register PNG decoder
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

const (
	imageMaxDim     = 800
	imageTempDir    = "/tmp/telegram-bridge"
	imageJPEGQuality = 85
)

// processPhoto downloads, saves, and resizes a photo from a Telegram update.
// Returns the local file path on success. The caller is responsible for
// deleting the file after use.
func (m *SessionManager) processPhoto(ctx context.Context, chatID, messageID int64, fileID string) (string, error) {
	dir := filepath.Join(imageTempDir, fmt.Sprintf("%d", chatID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.jpg", messageID))

	if err := downloadFile(ctx, m.proxyURL+"/file/"+fileID, path); err != nil {
		return "", err
	}

	if err := resizePhotoFile(path); err != nil {
		// Non-fatal: log and continue with the original file.
		log.Printf("[image] resize %s: %v", path, err)
	}

	return path, nil
}

// downloadFile fetches url and writes the response body to dest.
func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(dest)
		return fmt.Errorf("write %s: %w", dest, err)
	}
	return nil
}

// resizePhotoFile decodes a JPEG (or PNG) at path, scales it down so that the
// long edge is at most imageMaxDim pixels using nearest-neighbour sampling, and
// re-encodes it as JPEG in-place. Files already within the limit are unchanged.
func resizePhotoFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	src, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	b := src.Bounds()
	w := b.Dx()
	h := b.Dy()
	if w <= imageMaxDim && h <= imageMaxDim {
		return nil // already within limit
	}

	var newW, newH int
	if w >= h {
		newW = imageMaxDim
		newH = (h*imageMaxDim + w/2) / w
	} else {
		newH = imageMaxDim
		newW = (w*imageMaxDim + h/2) / h
	}
	if newH < 1 {
		newH = 1
	}
	if newW < 1 {
		newW = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	for y := 0; y < newH; y++ {
		srcY := b.Min.Y + y*h/newH
		for x := 0; x < newW; x++ {
			srcX := b.Min.X + x*w/newW
			r, g, bl, a := src.At(srcX, srcY).RGBA()
			// RGBA() returns 16-bit pre-multiplied values; convert to 8-bit.
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r >> 8),
				G: uint8(g >> 8),
				B: uint8(bl >> 8),
				A: uint8(a >> 8),
			})
		}
	}

	out, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer out.Close()
	return jpeg.Encode(out, dst, &jpeg.Options{Quality: imageJPEGQuality})
}
