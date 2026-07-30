package bridge

import (
	"testing"
)

// ── detectModelChange ──────────────────────────────────────────────────────────

func TestDetectModelChange_DirectSwitch(t *testing.T) {
	tests := []struct {
		input   string
		model   string
		delta   int
		cleaned string
	}{
		{"use opus", "claude-opus-4-6", 0, ""},
		{"switch to sonnet", "claude-sonnet-4-6", 0, ""},
		{"use haiku for this task", "claude-haiku-4-5", 0, "for this task"},
		{"use opus and then do something", "claude-opus-4-6", 0, "and then do something"},
		{"back to default", "__reset__", 0, ""},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			model, cleaned, delta := detectModelChange(tc.input)
			if model != tc.model {
				t.Errorf("model: got %q, want %q", model, tc.model)
			}
			if delta != tc.delta {
				t.Errorf("delta: got %d, want %d", delta, tc.delta)
			}
			if tc.cleaned != "" && cleaned != tc.cleaned {
				// Only check if cleaned should be non-empty
				t.Errorf("cleaned: got %q, want to contain %q", cleaned, tc.cleaned)
			}
		})
	}
}

func TestDetectModelChange_TierEscalation(t *testing.T) {
	gotModel, _, delta := detectModelChange("think harder")
	if delta != 1 {
		t.Errorf("delta: got %d, want 1", delta)
	}
	if gotModel != "" {
		t.Errorf("model: got %q, want empty (tier change)", gotModel)
	}
}

func TestDetectModelChange_TierDeEscalation(t *testing.T) {
	_, _, delta := detectModelChange("keep it simple")
	if delta != -1 {
		t.Errorf("delta: got %d, want -1", delta)
	}
}

func TestDetectModelChange_NoMatch(t *testing.T) {
	model, cleaned, delta := detectModelChange("write a function that sorts a list")
	if model != "" {
		t.Errorf("model: got %q, want empty", model)
	}
	if delta != 0 {
		t.Errorf("delta: got %d, want 0", delta)
	}
	if cleaned != "write a function that sorts a list" {
		t.Errorf("cleaned: got %q, want original text", cleaned)
	}
}

// ── detectCancelIntent ─────────────────────────────────────────────────────────

func TestDetectCancelIntent(t *testing.T) {
	tests := []struct {
		input         string
		isCancelOnly  bool
		hasRemainder  bool
	}{
		{"cancel", true, false},
		{"stop", true, false},
		{"abort", true, false},
		{"cancel that", true, false},
		{"stop what you are doing", true, false},
		{"scratch that", true, false},
		{"cancel and do something else", false, true},
		{"stop that please", false, true},
		{"never mind", true, false},
		{"nevermind that", true, false},
		{"hello world", false, false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			isCancel, remainder := detectCancelIntent(tc.input)
			if isCancel != tc.isCancelOnly {
				t.Errorf("isCancelOnly: got %v, want %v (remainder=%q)", isCancel, tc.isCancelOnly, remainder)
			}
			hasRem := remainder != ""
			if hasRem != tc.hasRemainder {
				t.Errorf("hasRemainder: got %v, want %v (remainder=%q)", hasRem, tc.hasRemainder, remainder)
			}
		})
	}
}

// ── detectNotifyIntent ─────────────────────────────────────────────────────────

func TestDetectNotifyIntent(t *testing.T) {
	tests := []struct {
		input string
		mode  string
	}{
		{"quiet", "summary"},
		{"silent", "summary"},
		{"just tell me when done", "summary"},
		{"be quiet", "quiet"},
		{"no updates", "quiet"},
		{"show everything", "live"},
		{"keep me posted", "live"},
		{"hello world", ""},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			mode, _ := detectNotifyIntent(tc.input)
			if mode != tc.mode {
				t.Errorf("mode: got %q, want %q", mode, tc.mode)
			}
		})
	}
}

// ── detectCostIntent ───────────────────────────────────────────────────────────

func TestDetectCostIntent(t *testing.T) {
	tests := []struct {
		input   string
		isQuery bool
	}{
		{"how much", true},
		{"what is the cost", true},
		{"show cost", true},
		{"total cost", true},
		{"how much has this cost so far", true},
		{"how much money is in my bank account in total", false}, // too much remainder
		{"hello world", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := detectCostIntent(tc.input)
			if got != tc.isQuery {
				t.Errorf("detectCostIntent(%q) = %v, want %v", tc.input, got, tc.isQuery)
			}
		})
	}
}

// ── detectStatusIntent ─────────────────────────────────────────────────────────

func TestDetectStatusIntent(t *testing.T) {
	tests := []struct {
		input   string
		isQuery bool
	}{
		{"what are you doing", true},
		{"show status", true},
		{"are you busy", true},
		{"whats running", true},
		{"session info", true},
		{"what are you doing right now with the codebase", false},
		{"hello world", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := detectStatusIntent(tc.input)
			if got != tc.isQuery {
				t.Errorf("detectStatusIntent(%q) = %v, want %v", tc.input, got, tc.isQuery)
			}
		})
	}
}

// ── detectCloseIntent ──────────────────────────────────────────────────────────

func TestDetectCloseIntent(t *testing.T) {
	tests := []struct {
		input    string
		detected bool
	}{
		{"close this session", true},
		{"end session", true},
		{"we are done", true},
		{"all done", true},
		{"wrap up", true},
		{"thats all for now", true},
		{"finished", true}, // short enough to trigger
		{"finished implementing the new feature and all tests pass", false}, // too long
		{"hello world", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			detected, _ := detectCloseIntent(tc.input)
			if detected != tc.detected {
				t.Errorf("detectCloseIntent(%q) = %v, want %v", tc.input, detected, tc.detected)
			}
		})
	}
}

// ── detectTimeoutIntent ────────────────────────────────────────────────────────

func TestDetectTimeoutIntent_NoLimit(t *testing.T) {
	phrases := []string{
		"no timeout",
		"let it run",
		"run as long as needed",
		"no time limit",
		"run indefinitely",
		"dont time out",
		"don't time out",
	}
	for _, p := range phrases {
		t.Run(p, func(t *testing.T) {
			intent := detectTimeoutIntent(p)
			if !intent.detected {
				t.Errorf("expected detection for %q", p)
			}
			if intent.timeoutSec != 0 {
				t.Errorf("timeoutSec: got %d, want 0 (no limit)", intent.timeoutSec)
			}
		})
	}
}

func TestDetectTimeoutIntent_WithDuration(t *testing.T) {
	intent := detectTimeoutIntent("set timeout to 600")
	if !intent.detected {
		t.Fatal("expected detection")
	}
	if intent.timeoutSec != 600 {
		t.Errorf("timeoutSec: got %d, want 600", intent.timeoutSec)
	}
}

func TestDetectTimeoutIntent_NoMatch(t *testing.T) {
	intent := detectTimeoutIntent("write a test")
	if intent.detected {
		t.Error("should not detect timeout intent")
	}
}

// ── detectModelQueryIntent ─────────────────────────────────────────────────────

func TestDetectModelQueryIntent(t *testing.T) {
	tests := []struct {
		input   string
		isQuery bool
	}{
		{"what model are you using", true},
		{"which model", true},
		{"current model", true},
		{"which model should I use for this complex analysis", false},
		{"hello world", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := detectModelQueryIntent(tc.input)
			if got != tc.isQuery {
				t.Errorf("detectModelQueryIntent(%q) = %v, want %v", tc.input, got, tc.isQuery)
			}
		})
	}
}

// ── detectHelpIntent ───────────────────────────────────────────────────────────

func TestDetectHelpIntent(t *testing.T) {
	tests := []struct {
		input   string
		isQuery bool
	}{
		{"help", true},
		{"what can you do", true},
		{"show commands", true},
		{"available commands", true},
		{"can you help me write some code", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := detectHelpIntent(tc.input)
			if got != tc.isQuery {
				t.Errorf("detectHelpIntent(%q) = %v, want %v", tc.input, got, tc.isQuery)
			}
		})
	}
}

// ── detectColorIntent ──────────────────────────────────────────────────────────

func TestDetectColorIntent(t *testing.T) {
	tests := []struct {
		input    string
		detected bool
		color    int
	}{
		{"mark as active", true, ColorActive},
		{"mark as complete", true, ColorComplete},
		{"mark as blocked", true, ColorBlocked},
		{"mark as error", true, ColorError},
		{"mark as review", true, ColorReview},
		{"mark as research", true, ColorResearch},
		{"color green", true, ColorComplete},
		{"color red", true, ColorError},
		{"color purple", true, ColorResearch},
		{"hello world", false, 0},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			detected, color := detectColorIntent(tc.input)
			if detected != tc.detected {
				t.Errorf("detected: got %v, want %v", detected, tc.detected)
			}
			if detected && color != tc.color {
				t.Errorf("color: got %d, want %d", color, tc.color)
			}
		})
	}
}

// ── tier escalation/de-escalation ──────────────────────────────────────────────

func TestApplyTierChange(t *testing.T) {
	// Escalate from sonnet to opus
	model, changed, msg := applyTierChange("claude-sonnet-4-6", 1)
	if !changed || model != "claude-opus-4-6" {
		t.Errorf("sonnet+1: got %q changed=%v msg=%q", model, changed, msg)
	}

	// Escalate from opus (already at top)
	model, changed, msg = applyTierChange("claude-opus-4-6", 1)
	if changed {
		t.Errorf("opus+1: should not change, got %q msg=%q", model, msg)
	}

	// De-escalate from sonnet to haiku
	model, changed, msg = applyTierChange("claude-sonnet-4-6", -1)
	if !changed || model != "claude-haiku-4-5" {
		t.Errorf("sonnet-1: got %q changed=%v msg=%q", model, changed, msg)
	}

	// De-escalate from haiku (already at bottom)
	model, changed, msg = applyTierChange("claude-haiku-4-5", -1)
	if changed {
		t.Errorf("haiku-1: should not change, got %q msg=%q", model, msg)
	}
}

// ── resolveSessionModel ────────────────────────────────────────────────────────

func TestResolveSessionModel(t *testing.T) {
	// Session model takes priority
	model := resolveSessionModel(&Session{Model: "claude-opus-4-6"}, &Group{DefaultModel: "claude-haiku-4-5"})
	if model != "claude-opus-4-6" {
		t.Errorf("session override: got %q", model)
	}

	// Group default when session has no model
	model = resolveSessionModel(&Session{}, &Group{DefaultModel: "claude-sonnet-4-6"})
	if model != "claude-sonnet-4-6" {
		t.Errorf("group default: got %q", model)
	}

	// Hardcoded fallback
	model = resolveSessionModel(nil, nil)
	if model != "claude-sonnet-4-6" {
		t.Errorf("fallback: got %q", model)
	}

	// Session model with empty string falls through to group
	model = resolveSessionModel(&Session{Model: ""}, &Group{DefaultModel: "claude-opus-4-6"})
	if model != "claude-opus-4-6" {
		t.Errorf("empty session model: got %q", model)
	}
}

// ── resolveToolRestrictions ────────────────────────────────────────────────────

func TestResolveToolRestrictions(t *testing.T) {
	// nil group
	allowed, disallowed := resolveToolRestrictions(nil)
	if allowed != "" || disallowed != "" {
		t.Errorf("nil group: got allowed=%q disallowed=%q", allowed, disallowed)
	}

	// Empty group
	allowed, disallowed = resolveToolRestrictions(&Group{})
	if allowed != "" || disallowed != "" {
		t.Errorf("empty group: got allowed=%q disallowed=%q", allowed, disallowed)
	}

	// With restrictions
	allowed, disallowed = resolveToolRestrictions(&Group{
		AllowedTools:    `["Read","Grep","Glob"]`,
		DisallowedTools: `["Bash","Edit"]`,
	})
	if allowed != "Read,Grep,Glob" {
		t.Errorf("allowed: got %q", allowed)
	}
	if disallowed != "Bash,Edit" {
		t.Errorf("disallowed: got %q", disallowed)
	}

	// Empty JSON arrays
	allowed, disallowed = resolveToolRestrictions(&Group{
		AllowedTools:    "[]",
		DisallowedTools: "[]",
	})
	if allowed != "" || disallowed != "" {
		t.Errorf("empty arrays: got allowed=%q disallowed=%q", allowed, disallowed)
	}
}

// ── removePhrase ────────────────────────────────────────────────────────────────

func TestRemovePhrase(t *testing.T) {
	tests := []struct {
		text   string
		phrase string
		result string
	}{
		{"use opus and do something", "use opus", "and do something"},
		{"Use Opus and do something", "use opus", "and do something"},
		{"hello world", "not found", "hello world"},
		{"use opus", "use opus", ""},
	}

	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			result := removePhrase(tc.text, tc.phrase)
			if result != tc.result {
				t.Errorf("removePhrase(%q, %q) = %q, want %q", tc.text, tc.phrase, result, tc.result)
			}
		})
	}
}

