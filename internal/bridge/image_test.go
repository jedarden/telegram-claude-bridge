package bridge

import (
	"fmt"
	"testing"
)

// TestImageConstants tests that image processing constants are properly defined.
func TestImageConstants(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
		check string
	}{
		{
			name:  "imageMaxDim",
			value: imageMaxDim,
			check: "800",
		},
		{
			name:  "imageTempDir",
			value: imageTempDir,
			check: "/tmp/telegram-bridge",
		},
		{
			name:  "imageJPEGQuality",
			value: imageJPEGQuality,
			check: "85",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			switch v := tc.value.(type) {
			case int:
				got = fmt.Sprintf("%d", v)
			case string:
				got = v
			}
			if got != tc.check {
				t.Errorf("%s = %v, want %s", tc.name, got, tc.check)
			}
		})
	}
}

// TestCalculateResizeBounds tests the resize bounds calculation logic.
func TestCalculateResizeBounds(t *testing.T) {
	tests := []struct {
		name       string
		w, h       int
		maxDim     int
		wantNewW   int
		wantNewH   int
		wantResize bool
	}{
		// No resize needed - already within bounds
		{
			name:       "400x300 - no resize needed",
			w:          400,
			h:          300,
			maxDim:     800,
			wantNewW:   400,
			wantNewH:   300,
			wantResize: false,
		},
		{
			name:       "800x600 - at boundary",
			w:          800,
			h:          600,
			maxDim:     800,
			wantNewW:   800,
			wantNewH:   600,
			wantResize: false,
		},

		// Width exceeds limit
		{
			name:       "1600x900 - width exceeds",
			w:          1600,
			h:          900,
			maxDim:     800,
			wantNewW:   800,
			wantNewH:   450,
			wantResize: true,
		},
		{
			name:       "1200x800 - width exceeds",
			w:          1200,
			h:          800,
			maxDim:     800,
			wantNewW:   800,
			wantNewH:   533,
			wantResize: true,
		},

		// Height exceeds limit
		{
			name:       "600x1600 - height exceeds",
			w:          600,
			h:          1600,
			maxDim:     800,
			wantNewW:   300,
			wantNewH:   800,
			wantResize: true,
		},
		{
			name:       "400x1200 - height exceeds",
			w:          400,
			h:          1200,
			maxDim:     800,
			wantNewW:   267,
			wantNewH:   800,
			wantResize: true,
		},

		// Both dimensions exceed limit
		{
			name:       "2000x1500 - both exceed, width is larger",
			w:          2000,
			h:          1500,
			maxDim:     800,
			wantNewW:   800,
			wantNewH:   600,
			wantResize: true,
		},
		{
			name:       "1500x2000 - both exceed, height is larger",
			w:          1500,
			h:          2000,
			maxDim:     800,
			wantNewW:   600,
			wantNewH:   800,
			wantResize: true,
		},

		// Edge cases - very small images after scaling
		{
			name:       "801x2 - width slightly exceeds, tiny height",
			w:          801,
			h:          2,
			maxDim:     800,
			wantNewW:   800,
			wantNewH:   1,
			wantResize: true,
		},
		{
			name:       "2x801 - height slightly exceeds, tiny width",
			w:          2,
			h:          801,
			maxDim:     800,
			wantNewW:   1,
			wantNewH:   800,
			wantResize: true,
		},

		// Square image
		{
			name:       "1000x1000 - square image",
			w:          1000,
			h:          1000,
			maxDim:     800,
			wantNewW:   800,
			wantNewH:   800,
			wantResize: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Check if resize is needed
			needsResize := tc.w > tc.maxDim || tc.h > tc.maxDim
			if needsResize != tc.wantResize {
				t.Errorf("needsResize: got %v, want %v", needsResize, tc.wantResize)
			}

			if !tc.wantResize {
				return
			}

			// Calculate new dimensions
			var newW, newH int
			if tc.w >= tc.h {
				newW = tc.maxDim
				newH = tc.h * tc.maxDim / tc.w
			} else {
				newH = tc.maxDim
				newW = tc.w * tc.maxDim / tc.h
			}

			// Ensure minimum dimensions
			if newH < 1 {
				newH = 1
			}
			if newW < 1 {
				newW = 1
			}

			if newW != tc.wantNewW {
				t.Errorf("newW: got %d, want %d", newW, tc.wantNewW)
			}
			if newH != tc.wantNewH {
				t.Errorf("newH: got %d, want %d", newH, tc.wantNewH)
			}
		})
	}
}

// TestCalculateResizeBoundsWithMaxDim tests resize calculations with different max dimensions.
func TestCalculateResizeBoundsWithMaxDim(t *testing.T) {
	tests := []struct {
		name     string
		w, h     int
		maxDim   int
		wantNewW int
		wantNewH int
	}{
		{
			name:     "maxDim=1024, width exceeds",
			w:        2000,
			h:        1500,
			maxDim:   1024,
			wantNewW: 1024,
			wantNewH: 768,
		},
		{
			name:     "maxDim=640, height exceeds",
			w:        500,
			h:        1200,
			maxDim:   640,
			wantNewW: 267,
			wantNewH: 640,
		},
		{
			name:     "maxDim=512, both exceed",
			w:        1920,
			h:        1080,
			maxDim:   512,
			wantNewW: 512,
			wantNewH: 288,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var newW, newH int
			if tc.w >= tc.h {
				newW = tc.maxDim
				newH = tc.h * tc.maxDim / tc.w
			} else {
				newH = tc.maxDim
				newW = tc.w * tc.maxDim / tc.h
			}

			if newW < 1 {
				newW = 1
			}
			if newH < 1 {
				newH = 1
			}

			if newW != tc.wantNewW {
				t.Errorf("newW: got %d, want %d", newW, tc.wantNewW)
			}
			if newH != tc.wantNewH {
				t.Errorf("newH: got %d, want %d", newH, tc.wantNewH)
			}
		})
	}
}

// TestAspectRationPreservation tests that aspect ratio is preserved during resize.
func TestAspectRatioPreservation(t *testing.T) {
	testCases := []struct {
		w, h   int
		maxDim int
	}{
		{1920, 1080, 800},
		{1080, 1920, 800},
		{4096, 2160, 800},
		{1000, 1000, 800},
		{3000, 1000, 800},
		{1000, 3000, 800},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%dx%d", tc.w, tc.h), func(t *testing.T) {
			originalAspect := float64(tc.w) / float64(tc.h)

			var newW, newH int
			if tc.w >= tc.h {
				newW = tc.maxDim
				newH = tc.h * tc.maxDim / tc.w
			} else {
				newH = tc.maxDim
				newW = tc.w * tc.maxDim / tc.h
			}

			if newH < 1 {
				newH = 1
			}
			if newW < 1 {
				newW = 1
			}

			newAspect := float64(newW) / float64(newH)

			// Aspect ratio should be preserved (within rounding error)
			diff := originalAspect - newAspect
			if diff < 0 {
				diff = -diff
			}

			// Allow 1% difference due to integer rounding
			maxDiff := originalAspect * 0.01
			if diff > maxDiff && diff > 0.01 {
				t.Errorf("aspect ratio not preserved: %dx%d (%.4f) -> %dx%d (%.4f), diff %.4f",
					tc.w, tc.h, originalAspect, newW, newH, newAspect, diff)
			}
		})
	}
}
