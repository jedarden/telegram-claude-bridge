package telegram

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

type normalizeFixture struct {
	Raw  json.RawMessage `json:"raw"`
	Want contract.Update `json:"want"`
}

func TestNormalizeUpdate_JSONFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatalf("find fixtures: %v", err)
	}
	sort.Strings(paths)
	if len(paths) != 4 {
		t.Fatalf("found %d fixtures, want 4", len(paths))
	}

	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}

			var fixture normalizeFixture
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}

			var raw Update
			if err := json.Unmarshal(fixture.Raw, &raw); err != nil {
				t.Fatalf("decode raw Telegram update: %v", err)
			}
			got, err := NormalizeUpdate(raw)
			if err != nil {
				t.Fatalf("normalize update: %v", err)
			}
			if got == nil {
				t.Fatal("normalize update returned nil")
			}

			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("encode normalized update: %v", err)
			}
			wantJSON, err := json.Marshal(fixture.Want)
			if err != nil {
				t.Fatalf("encode expected update: %v", err)
			}
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("normalized envelope mismatch:\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}
