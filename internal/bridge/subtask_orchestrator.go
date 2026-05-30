// Package bridge implements the bridge-side components that connect to the proxy.
package bridge

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SubtaskRequest contains parameters for spawning parallel subtasks.
type SubtaskRequest struct {
	ChatID   int64    // Parent chat ID
	ThreadID int64    // Parent thread ID
	MsgID    int64    // Parent message ID (for replies)
	Prompts  []string // Prompts to execute in parallel (max 5)
	Group    *Group   // Group configuration
	Session  *Session // Optional parent session to resume
}

// SubtaskOrchestrator manages fan-out/fan-in of parallel Claude invocations.
// Each subtask runs in its own goroutine with a fresh session, and results
// are posted back to the originating topic as they complete (non-blocking fan-in).
type SubtaskOrchestrator struct {
	db         *DB
	sender     *Sender
	sessionMgr *SessionManager // For PTYManager access and injecting worker results
}

// NewSubtaskOrchestrator creates a new SubtaskOrchestrator.
func NewSubtaskOrchestrator(db *DB, sender *Sender, sessionMgr *SessionManager) *SubtaskOrchestrator {
	return &SubtaskOrchestrator{
		db:         db,
		sender:     sender,
		sessionMgr: sessionMgr,
	}
}

// Run executes N parallel subtasks and posts results as they complete.
// Returns an error if setup fails, otherwise runs asynchronously.
func (o *SubtaskOrchestrator) Run(ctx context.Context, req SubtaskRequest) error {
	if len(req.Prompts) == 0 {
		return fmt.Errorf("no prompts provided")
	}
	if len(req.Prompts) > 5 {
		return fmt.Errorf("maximum 5 prompts allowed")
	}

	// Enforce per-group max concurrent subtasks
	maxConcurrent := req.Group.MaxSubtasks
	if maxConcurrent <= 0 {
		maxConcurrent = 5 // Default limit
	}
	if len(req.Prompts) > maxConcurrent {
		return fmt.Errorf("too many prompts: group max_subtasks is %d", maxConcurrent)
	}

	// Generate subtask IDs and insert into database
	subtaskIDs := make([]string, len(req.Prompts))
	for i, prompt := range req.Prompts {
		subtaskIDs[i] = uuid.New().String()
		subtask := &Subtask{
			ID:          subtaskIDs[i],
			ChatID:      req.ChatID,
			ThreadID:    req.ThreadID,
			ParentMsgID: req.MsgID,
			Prompt:      prompt,
			Status:      "running",
			StartedAt:   time.Now().UTC(),
		}
		if req.Session != nil {
			subtask.SessionID = req.Session.SessionID
		}
		if err := o.db.CreateSubtask(ctx, subtask); err != nil {
			return fmt.Errorf("create subtask: %w", err)
		}
	}

	// Spawn goroutine to run all subtasks and collect results
	go o.runSubtasks(ctx, req, subtaskIDs)

	return nil
}

// runSubtasks executes all subtasks in parallel and posts results as they complete.
// This runs in a separate goroutine so the caller can return immediately.
func (o *SubtaskOrchestrator) runSubtasks(ctx context.Context, req SubtaskRequest, subtaskIDs []string) {
	var wg sync.WaitGroup
	results := make(chan *subtaskResult, len(subtaskIDs))

	// Spawn a goroutine for each subtask
	for i, prompt := range req.Prompts {
		wg.Add(1)
		go func(idx int, subtaskID string, prompt string) {
			defer wg.Done()

			// Run the subtask with its own context
			result := o.executeSubtask(ctx, req, subtaskID, prompt)
			results <- result

			// Update database with result
			if result.Error != nil {
				_ = o.db.UpdateSubtask(ctx, subtaskID, "error", "", result.Error.Error())
			} else {
				_ = o.db.UpdateSubtask(ctx, subtaskID, "complete", result.Output, "")
			}
		}(i, subtaskIDs[i], prompt)
	}

	// Close results channel when all goroutines complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Fan-in: post results as they arrive (non-blocking)
	completed := 0
	for result := range results {
		completed++
		o.postResult(ctx, req, result, completed, len(subtaskIDs))
	}
}

// subtaskResult holds the output from a single subtask execution.
type subtaskResult struct {
	SubtaskID string
	Output    string
	Error     error
}

// executeSubtask runs a single Claude invocation for a subtask via an ephemeral tmux pane.
func (o *SubtaskOrchestrator) executeSubtask(ctx context.Context, req SubtaskRequest, subtaskID, prompt string) *subtaskResult {
	ptyMgr := o.sessionMgr.PTYManager()
	paneName := fmt.Sprintf("st-%s-%d", subtaskID[:8], time.Now().UnixNano())

	args := []string{
		"--dangerously-skip-permissions",
		"--model", resolveSessionModel(req.Session, req.Group),
	}

	// Add tool restrictions if configured
	allowed, disallowed := resolveToolRestrictions(req.Group)
	if allowed != "" {
		args = append(args, "--allowed-tools", allowed)
	}
	if disallowed != "" {
		args = append(args, "--disallowed-tools", disallowed)
	}

	timeoutSec := req.Group.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 1800 // Default 30 minutes
	}
	subCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	paneTarget, err := ptyMgr.SpawnPane(paneName, req.Group.CWD, args)
	if err != nil {
		return &subtaskResult{SubtaskID: subtaskID, Error: fmt.Errorf("spawn pane: %w", err)}
	}
	defer ptyMgr.KillPane(paneTarget)

	if err := ptyMgr.WaitForStartup(paneTarget); err != nil {
		return &subtaskResult{SubtaskID: subtaskID, Error: fmt.Errorf("startup: %w", err)}
	}
	if err := ptyMgr.InjectPrompt(paneTarget, prompt); err != nil {
		return &subtaskResult{SubtaskID: subtaskID, Error: fmt.Errorf("inject prompt: %w", err)}
	}
	result, err := ptyMgr.WaitForResponse(subCtx, paneTarget, nil)
	if err != nil {
		return &subtaskResult{SubtaskID: subtaskID, Error: fmt.Errorf("wait response: %w", err)}
	}

	return &subtaskResult{SubtaskID: subtaskID, Output: result}
}

// postResult sends a single subtask result to the topic and stores it for injection.
func (o *SubtaskOrchestrator) postResult(ctx context.Context, req SubtaskRequest, result *subtaskResult, completed, total int) {
	tidPtr := &req.ThreadID

	// Store the result for injection into the next orchestrator prompt
	model := resolveSessionModel(req.Session, req.Group)
	if result.Error != nil {
		o.sessionMgr.AddPendingWorkerResult(req.ChatID, req.ThreadID, WorkerResult{
			Index:  completed,
			Model:  model,
			Result: "",
			Error:  result.Error.Error(),
		})
	} else {
		o.sessionMgr.AddPendingWorkerResult(req.ChatID, req.ThreadID, WorkerResult{
			Index:  completed,
			Model:  model,
			Result: result.Output,
		})
	}

	if result.Error != nil {
		// Post error result
		msg := fmt.Sprintf("⚠️ Subtask %d/%d failed:\n\n%v", completed, total, result.Error)
		if err := o.sender.SendResponse(ctx, req.ChatID, tidPtr, req.MsgID, msg); err != nil {
			log.Printf("[subtask_orchestrator] send error result: %v", err)
		}
		return
	}

	// Post successful result
	msg := fmt.Sprintf("✓ Subtask %d/%d complete:\n\n%s", completed, total, result.Output)
	if err := o.sender.SendResponse(ctx, req.ChatID, tidPtr, req.MsgID, msg); err != nil {
		log.Printf("[subtask_orchestrator] send result: %v", err)
	}

	// If all subtasks complete, post a summary
	if completed == total {
		summary := fmt.Sprintf("✅ All %d subtasks complete.", total)
		if err := o.sender.SendResponse(ctx, req.ChatID, tidPtr, req.MsgID, summary); err != nil {
			log.Printf("[subtask_orchestrator] send summary: %v", err)
		}
	}
}

// ListRunningSubtasks returns all running subtasks for a topic.
func (o *SubtaskOrchestrator) ListRunningSubtasks(ctx context.Context, chatID, threadID int64) ([]*Subtask, error) {
	return o.db.ListSubtasksByStatus(ctx, chatID, threadID, "running")
}

// CancelSubtasks cancels all running subtasks for a topic.
// Note: this is a best-effort cancellation - it marks them as cancelled in the DB
// but cannot actually stop running goroutines.
func (o *SubtaskOrchestrator) CancelSubtasks(ctx context.Context, chatID, threadID int64) (int, error) {
	subtasks, err := o.db.ListSubtasksByStatus(ctx, chatID, threadID, "running")
	if err != nil {
		return 0, err
	}

	count := 0
	for _, st := range subtasks {
		if err := o.db.UpdateSubtask(ctx, st.ID, "cancelled", "", "Cancelled by user"); err != nil {
			log.Printf("[subtask_orchestrator] cancel subtask %s: %v", st.ID, err)
			continue
		}
		count++
	}

	return count, nil
}
