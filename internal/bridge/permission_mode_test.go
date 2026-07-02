package bridge

import (
	"reflect"
	"testing"
)

// TestResolvePermissionMode verifies that resolvePermissionMode returns the
// configured permission mode from the group, falling back to the default.
func TestResolvePermissionMode(t *testing.T) {
	tests := []struct {
		name           string
		group          *Group
		expectedMode   string
	}{
		{
			name: "group with bypassPermissions mode",
			group: &Group{
				PermissionMode: "bypassPermissions",
			},
			expectedMode: "bypassPermissions",
		},
		{
			name: "group with acceptEdits mode",
			group: &Group{
				PermissionMode: "acceptEdits",
			},
			expectedMode: "acceptEdits",
		},
		{
			name: "group with plan mode",
			group: &Group{
				PermissionMode: "plan",
			},
			expectedMode: "plan",
		},
		{
			name: "group with dontAsk mode",
			group: &Group{
				PermissionMode: "dontAsk",
			},
			expectedMode: "dontAsk",
		},
		{
			name:           "nil group - uses default bypassPermissions",
			group:          nil,
			expectedMode:   "bypassPermissions",
		},
		{
			name: "group with empty permission_mode - uses default bypassPermissions",
			group: &Group{
				PermissionMode: "",
			},
			expectedMode: "bypassPermissions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := resolvePermissionMode(tc.group)
			if result != tc.expectedMode {
				t.Errorf("resolvePermissionMode() = %q, want %q", result, tc.expectedMode)
			}
		})
	}
}

// TestResolvePermissionArgs verifies that resolvePermissionArgs returns the
// correct command-line arguments for each permission mode.
func TestResolvePermissionArgs(t *testing.T) {
	tests := []struct {
		name            string
		group           *Group
		expectedArgs    []string
	}{
		{
			name: "bypassPermissions mode - uses --dangerously-skip-permissions",
			group: &Group{
				PermissionMode: "bypassPermissions",
			},
			expectedArgs: []string{"--dangerously-skip-permissions"},
		},
		{
			name: "acceptEdits mode - uses --permission-mode acceptEdits",
			group: &Group{
				PermissionMode: "acceptEdits",
			},
			expectedArgs: []string{"--permission-mode", "acceptEdits"},
		},
		{
			name: "plan mode - uses --permission-mode plan",
			group: &Group{
				PermissionMode: "plan",
			},
			expectedArgs: []string{"--permission-mode", "plan"},
		},
		{
			name: "dontAsk mode - uses --permission-mode dontAsk",
			group: &Group{
				PermissionMode: "dontAsk",
			},
			expectedArgs: []string{"--permission-mode", "dontAsk"},
		},
		{
			name:          "nil group - uses default bypassPermissions",
			group:         nil,
			expectedArgs:  []string{"--dangerously-skip-permissions"},
		},
		{
			name: "empty permission_mode - uses default bypassPermissions",
			group: &Group{
				PermissionMode: "",
			},
			expectedArgs: []string{"--dangerously-skip-permissions"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := resolvePermissionArgs(tc.group)
			if !reflect.DeepEqual(result, tc.expectedArgs) {
				t.Errorf("resolvePermissionArgs() = %v, want %v", result, tc.expectedArgs)
			}
		})
	}
}

// TestDefaultPermissionMode verifies that the default permission mode
// constant is set to bypassPermissions.
func TestDefaultPermissionMode(t *testing.T) {
	if defaultPermissionMode != "bypassPermissions" {
		t.Errorf("defaultPermissionMode = %q, want %q", defaultPermissionMode, "bypassPermissions")
	}
}
