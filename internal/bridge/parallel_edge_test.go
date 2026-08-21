// Package bridge provides edge-case integration tests for the /parallel
// command: shapes of args that exercise the boundary between cmdParallel and
// splitParallelPrompts (edge delimiters, empty segments, CRLF lookalikes) and
// their effect on the handler's reply and the persisted subtask rows. The
// unit-level splitter tests cover splitParallelPrompts in isolation; these
// tests pin the same behavior through the full handler path.
package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// newParallelEdgeHandler wires a handler whose /parallel path can reach
// orchestrator.Run. The session manager is tmux-backed (fixture tmux via
// PATH shadowing) because a successful Run spawns goroutines that start
// sessions; without the fixture those goroutines would reach real tmux.
// TimeoutSec is kept at 1s so any straggler goroutine's context expires
// promptly after the test asserts.
func newParallelEdgeHandler(t *testing.T) (*CommandHandler, *DB, *Group) {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		TimeoutSec:   1,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "sess-parallel-edge",
		CWD:       "/tmp/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender := newIntegrationTestSender(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)
	return h, db, group
}

// TestCommandHandler_cmdParallel_EdgeDelimiters runs delimiter edge shapes
// through the handler and verifies the reply, the persisted subtask count,
// and the exact prompt set stored. splitParallelPrompts unit tests cover the
// splitting in isolation; this pins the behavior end-to-end: a delimiter at
// either edge or consecutive delimiters must not produce empty subtasks.
func TestCommandHandler_cmdParallel_EdgeDelimiters(t *testing.T) {
	tests := []struct {
		name        string
		args        string
		wantReply   string
		wantPrompts []string
	}{
		{
			name:        "leading and trailing delimiters are consumed",
			args:        "---\nanalyze the diff\n---",
			wantReply:   "Started 1 parallel subtask(s)",
			wantPrompts: []string{"analyze the diff"},
		},
		{
			name:        "consecutive delimiters drop the empty segment",
			args:        "summarize a\n---\n---\nsummarize b",
			wantReply:   "Started 2 parallel subtask(s)",
			wantPrompts: []string{"summarize a", "summarize b"},
		},
		{
			name:        "whitespace-only segment is dropped",
			args:        "task a\n---\n   \n---\ntask b",
			wantReply:   "Started 2 parallel subtask(s)",
			wantPrompts: []string{"task a", "task b"},
		},
		{
			name:        "tab-only segment is content that trims to empty and is dropped",
			args:        "task a\n---\n\t\n---\ntask b",
			wantReply:   "Started 2 parallel subtask(s)",
			wantPrompts: []string{"task a", "task b"},
		},
		{
			name:        "spaces around a delimiter still split",
			args:        "task a\n  ---  \ntask b",
			wantReply:   "Started 2 parallel subtask(s)",
			wantPrompts: []string{"task a", "task b"},
		},
		{
			name:        "bare delimiter without a newline is a prompt",
			args:        "---",
			wantReply:   "Started 1 parallel subtask(s)",
			wantPrompts: []string{"---"},
		},
		{
			name:        "CRLF lookalike delimiter is content",
			args:        "task one\r\n---\r\ntask two",
			wantReply:   "Started 1 parallel subtask(s)",
			wantPrompts: []string{"task one\r\n---\r\ntask two"},
		},
		{
			name:        "internal newlines are preserved in a single prompt",
			args:        "line one\nline two\nline three",
			wantReply:   "Started 1 parallel subtask(s)",
			wantPrompts: []string{"line one\nline two\nline three"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			h, db, group := newParallelEdgeHandler(t)

			threadID := int64(10)
			update := contract.Update{
				ChatID:    100,
				ThreadID:  &threadID,
				MessageID: 1000,
				FromUser:  contract.FromUser{ID: 12345},
				Content: &contract.Content{
					Text: &tt.args,
				},
			}

			reply, err := h.cmdParallel(ctx, update, group, tt.args)
			if err != nil {
				t.Fatalf("cmdParallel: %v", err)
			}
			if !containsSubstring(reply, tt.wantReply) {
				t.Errorf("reply = %q, want to contain %q", reply, tt.wantReply)
			}

			subtasks, err := db.ListSubtasks(ctx, 100, 10)
			if err != nil {
				t.Fatalf("ListSubtasks: %v", err)
			}
			if len(subtasks) != len(tt.wantPrompts) {
				t.Fatalf("expected %d subtasks, got %d (prompts: %v)", len(tt.wantPrompts), len(subtasks), subtasks)
			}
			got := map[string]bool{}
			for _, st := range subtasks {
				got[st.Prompt] = true
				// Status is deliberately not asserted: Run's background
				// goroutine may already have flipped a row to complete or
				// error before this check runs. Prompt, ParentMsgID, and
				// SessionID are written only by the synchronous CreateSubtask
				// and so are deterministic here.
				if st.ParentMsgID != 1000 {
					t.Errorf("subtask prompt %q ParentMsgID = %d, want 1000", st.Prompt, st.ParentMsgID)
				}
				if st.SessionID != "sess-parallel-edge" {
					t.Errorf("subtask prompt %q SessionID = %q, want 'sess-parallel-edge'", st.Prompt, st.SessionID)
				}
			}
			for _, want := range tt.wantPrompts {
				if !got[want] {
					t.Errorf("stored prompts = %v, want %q present", got, want)
				}
			}
		})
	}
}

// TestCommandHandler_cmdParallel_EmptySegmentsExcludedFromLimit pins the
// ordering of the split-filter-limit pipeline: the 5-prompt cap applies to
// non-empty prompts only. A batch with 6 raw segments where one is
// whitespace-only must be accepted as 5, while 6 non-empty prompts (plus an
// empty segment) must still be rejected.
func TestCommandHandler_cmdParallel_EmptySegmentsExcludedFromLimit(t *testing.T) {
	t.Run("empty segment does not trip the limit", func(t *testing.T) {
		ctx := context.Background()
		h, db, group := newParallelEdgeHandler(t)

		threadID := int64(10)
		text := "/parallel ..."
		update := contract.Update{
			ChatID:    100,
			ThreadID:  &threadID,
			MessageID: 1000,
			FromUser:  contract.FromUser{ID: 12345},
			Content: &contract.Content{
				Text: &text,
			},
		}

		// 6 raw segments; the whitespace-only one is filtered before the
		// cap, leaving exactly 5 prompts.
		args := "1\n---\n   \n---\n2\n---\n3\n---\n4\n---\n5"
		reply, err := h.cmdParallel(ctx, update, group, args)
		if err != nil {
			t.Fatalf("cmdParallel: %v", err)
		}
		if !containsSubstring(reply, "Started 5 parallel subtask(s)") {
			t.Errorf("reply = %q, want to contain 'Started 5 parallel subtask(s)'", reply)
		}

		subtasks, err := db.ListSubtasks(ctx, 100, 10)
		if err != nil {
			t.Fatalf("ListSubtasks: %v", err)
		}
		if len(subtasks) != 5 {
			t.Fatalf("expected 5 subtasks after empty-segment filtering, got %d", len(subtasks))
		}
		for _, st := range subtasks {
			if strings.TrimSpace(st.Prompt) == "" {
				t.Errorf("empty prompt persisted as subtask %s", st.ID)
			}
		}
	})

	t.Run("empty segment does not rescue an over-limit batch", func(t *testing.T) {
		ctx := context.Background()
		h, db, group := newParallelEdgeHandler(t)

		threadID := int64(10)
		text := "/parallel ..."
		update := contract.Update{
			ChatID:    100,
			ThreadID:  &threadID,
			MessageID: 1000,
			FromUser:  contract.FromUser{ID: 12345},
			Content: &contract.Content{
				Text: &text,
			},
		}

		// 7 raw segments, 6 non-empty: still over the cap.
		args := "1\n---\n2\n---\n3\n---\n4\n---\n5\n---\n6\n---\n   "
		reply, err := h.cmdParallel(ctx, update, group, args)
		if err != nil {
			t.Fatalf("cmdParallel: %v", err)
		}
		if !containsSubstring(reply, "Maximum 5 prompts") {
			t.Errorf("reply = %q, want to contain 'Maximum 5 prompts'", reply)
		}

		subtasks, err := db.ListSubtasks(ctx, 100, 10)
		if err != nil {
			t.Fatalf("ListSubtasks: %v", err)
		}
		if len(subtasks) != 0 {
			t.Errorf("expected 0 subtasks for rejected batch, got %d", len(subtasks))
		}
	})
}

// TestCommandHandler_Handle_ParallelCommand_EdgeDelimiters routes an
// edge-shaped /parallel through the full dispatch path (Handle → arg
// extraction → cmdParallel → orchestrator.Run) and verifies the reply posted
// to the proxy and the single persisted subtask. The existing dispatch test
// covers clean two-prompt input; this covers delimiters at both edges of the
// command text.
func TestCommandHandler_Handle_ParallelCommand_EdgeDelimiters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/tmp/test",
		DefaultModel: "claude-sonnet-4-6",
		MaxSubtasks:  5,
		TimeoutSec:   1,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "sess-dispatch-edge",
		CWD:       "/tmp/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	sender, rec := newRecordingProxy(t)
	sm := newTestSessionManagerWithTmux(t, db, sender)
	so := NewSubtaskOrchestrator(db, sender, sm)

	h := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	h.SetSubtaskOrchestrator(so)

	threadID := int64(10)
	text := "/parallel ---\ndo work\n---"
	update := parallelCommandUpdate(100, &threadID, 888, 12345, text)

	h.Handle(ctx, update, group)

	// The start reply must reach the proxy addressed to the topic.
	sends := rec.all()
	var replySend *recordedSend
	for i := range sends {
		if containsSubstring(sends[i].Body.Text, "Started 1 parallel subtask(s)") {
			replySend = &sends[i]
			break
		}
	}
	if replySend == nil {
		t.Fatalf("no /send request contained the parallel start reply; sends = %+v", sends)
	}
	if replySend.Body.ChatID != 100 {
		t.Errorf("reply ChatID = %d, want 100", replySend.Body.ChatID)
	}
	if replySend.Body.ThreadID == nil || *replySend.Body.ThreadID != 10 {
		t.Errorf("reply ThreadID = %v, want 10", replySend.Body.ThreadID)
	}

	// Both edge delimiters must be consumed: exactly one subtask, holding
	// the real prompt, attributed to the spawning message.
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 1 {
		t.Fatalf("expected 1 subtask from edge-delimited text, got %d", len(subtasks))
	}
	if subtasks[0].Prompt != "do work" {
		t.Errorf("subtask Prompt = %q, want 'do work'", subtasks[0].Prompt)
	}
	if subtasks[0].ParentMsgID != 888 {
		t.Errorf("subtask ParentMsgID = %d, want 888", subtasks[0].ParentMsgID)
	}
	// Status is not asserted: Run's background goroutine may have completed
	// the subtask against the fixture tmux before this check runs.
}
