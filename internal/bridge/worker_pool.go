package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// WorkerPool manages spawning and tracking of worker subprocesses.
// Workers are short-lived Claude CLI invocations dispatched by the orchestrator
// via the spawn_worker synthetic tool. Results are posted to Telegram and
// injected back into the orchestrator's context on the next invocation.
type WorkerPool struct {
	db         *DB
	sender     *Sender
	sessionMgr *SessionManager // For PTYManager access and injecting worker results

	mu        sync.Mutex
	nextIndex map[topicKey]int // monotonically increasing worker index per topic
}

// NewWorkerPool creates a new WorkerPool.
func NewWorkerPool(db *DB, sender *Sender, sessionMgr *SessionManager) *WorkerPool {
	return &WorkerPool{
		db:         db,
		sender:     sender,
		sessionMgr: sessionMgr,
		nextIndex:  make(map[topicKey]int),
	}
}

// spawnWorkerInput represents the parsed input for a spawn_worker synthetic tool call.
type spawnWorkerInput struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model,omitempty"`
}

// SpawnWorker handles a spawn_worker tool call from the orchestrator stream.
// It creates a DB record, spawns a goroutine to run the worker, and returns
// immediately with the worker ID and index for the tool_result.
func (wp *WorkerPool) SpawnWorker(
	ctx context.Context,
	chatID int64,
	threadID int64,
	parentMsgID int64,
	group *Group,
	inputJSON json.RawMessage,
) (workerID string, index int, retErr error) {
	// Parse input
	var input spawnWorkerInput
	if err := json.Unmarshal(inputJSON, &input); err != nil {
		return "", 0, fmt.Errorf("parse spawn_worker input: %w", err)
	}
	if input.Prompt == "" {
		return "", 0, fmt.Errorf("spawn_worker requires a non-empty prompt")
	}

	// Check concurrency limit
	running, err := wp.db.CountRunningWorkers(ctx, chatID, threadID)
	if err != nil {
		return "", 0, fmt.Errorf("count running workers: %w", err)
	}
	maxWorkers := group.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = 5
	}
	if running >= maxWorkers {
		return "", 0, fmt.Errorf("max workers (%d) already running for this topic", maxWorkers)
	}

	// Get next worker index for this topic
	key := topicKey{chatID: chatID, threadID: threadID}
	wp.mu.Lock()
	wp.nextIndex[key]++
	index = wp.nextIndex[key]
	wp.mu.Unlock()

	workerID = fmt.Sprintf("worker_%d_%d", threadID, time.Now().UnixNano())

	// Resolve model
	model := input.Model
	if model == "" {
		model = group.DefaultModel
		if model == "" {
			model = defaultSessionModel
		}
	}

	// Create DB record
	worker := &Worker{
		ID:        workerID,
		ChatID:    chatID,
		ThreadID:  threadID,
		ParentMsg: parentMsgID,
		Prompt:    input.Prompt,
		Model:     model,
		Status:    "running",
	}
	if err := wp.db.CreateWorker(ctx, worker); err != nil {
		return "", 0, fmt.Errorf("create worker record: %w", err)
	}

	// Spawn goroutine to run the worker
	go wp.runWorker(worker, input.Prompt, model, group, index)

	return workerID, index, nil
}

// runWorker executes a fresh Claude session via an ephemeral tmux pane.
func (wp *WorkerPool) runWorker(
	worker *Worker,
	prompt, model string,
	group *Group,
	index int,
) {
	ctx := context.Background()
	ptyMgr := wp.sessionMgr.PTYManager()

	paneName := fmt.Sprintf("w-%s-%d", worker.ID[:8], time.Now().UnixNano())
	permArgs := resolvePermissionArgs(group)
	args := append(permArgs,
		"--model", model,
	)

	// Add tool restrictions; always disallow spawn_worker (depth limit = 1)
	allowed, disallowed := resolveToolRestrictions(group)
	if allowed != "" {
		args = append(args, "--allowed-tools", allowed)
	}
	if disallowed != "" {
		disallowed += ",spawn_worker"
	} else {
		disallowed = "spawn_worker"
	}
	args = append(args, "--disallowed-tools", disallowed)

	paneTarget, err := ptyMgr.SpawnPane(paneName, group.CWD, args)
	if err != nil {
		wp.finishWorker(worker, index, "", fmt.Sprintf("spawn pane: %v", err))
		return
	}
	defer ptyMgr.KillPane(paneTarget)

	if err := ptyMgr.WaitForStartup(paneTarget); err != nil {
		wp.finishWorker(worker, index, "", fmt.Sprintf("startup: %v", err))
		return
	}
	preInjectScreen, _ := ptyMgr.CaptureScreen(paneTarget)
	if err := ptyMgr.InjectPrompt(paneTarget, prompt); err != nil {
		wp.finishWorker(worker, index, "", fmt.Sprintf("inject prompt: %v", err))
		return
	}

	result, err := ptyMgr.WaitForResponse(ctx, paneTarget, preInjectScreen, nil)
	if err != nil {
		wp.finishWorker(worker, index, "", fmt.Sprintf("wait response: %v", err))
		return
	}

	wp.finishWorker(worker, index, result, "")
}

// finishWorker updates the worker record, posts result to Telegram, and
// stores the result for injection into the orchestrator's next prompt.
func (wp *WorkerPool) finishWorker(
	worker *Worker,
	index int,
	result, errMsg string,
) {
	ctx := context.Background()

	status := "done"
	if errMsg != "" {
		status = "failed"
	}

	// Update DB
	if err := wp.db.UpdateWorker(ctx, worker.ID, status, result, errMsg); err != nil {
		log.Printf("[worker_pool] update worker %s status: %v", worker.ID, err)
	}

	// Truncate result for Telegram display
	displayResult := result
	if len(displayResult) > 2000 {
		displayResult = displayResult[:2000] + "..."
	}

	// Build Telegram message
	var msg string
	if status == "done" {
		msg = fmt.Sprintf("⚙️ Worker [%d] complete: %s", index, displayResult)
	} else {
		msg = fmt.Sprintf("⚙️ Worker [%d] FAILED: %s", index, errMsg)
		if len(msg) > 4096 {
			msg = msg[:4096]
		}
	}

	// Post to Telegram thread
	threadID := worker.ThreadID
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := wp.sender.SendResponse(sendCtx, worker.ChatID, &threadID, 0, msg); err != nil {
		log.Printf("[worker_pool] post worker %s result to telegram: %v", worker.ID, err)
	}

	// Store result for injection into orchestrator's next prompt
	wp.sessionMgr.AddPendingWorkerResult(worker.ChatID, worker.ThreadID, WorkerResult{
		Index:  index,
		Model:  worker.Model,
		Result: result,
		Error:  errMsg,
	})
}
