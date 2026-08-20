// Package bridge implements the bridge-side components that connect to the proxy.
package bridge

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// streamDebounce is the minimum interval between progressive Telegram edits for job output.
	jobStreamDebounce = 1 * time.Second
)

// BackgroundJobManager manages background shell jobs.
type BackgroundJobManager struct {
	db     *DB
	sender *Sender

	mu    sync.Mutex
	jobs  map[string]*BackgroundJob   // ID -> Job
	cmds  map[string]*exec.Cmd        // ID -> running exec.Cmd
}

// NewBackgroundJobManager creates a new BackgroundJobManager.
func NewBackgroundJobManager(db *DB, sender *Sender) *BackgroundJobManager {
	mgr := &BackgroundJobManager{
		db:     db,
		sender: sender,
		jobs:   make(map[string]*BackgroundJob),
		cmds:   make(map[string]*exec.Cmd),
	}
	// Load running jobs from DB and mark them as interrupted
	mgr.recoverRunningJobs()
	return mgr
}

// recoverRunningJobs loads jobs with status=running from the database and marks
// them as interrupted (since the bridge restarted).
func (m *BackgroundJobManager) recoverRunningJobs() {
	ctx := context.Background()
	jobs, err := m.db.ListBackgroundJobsByStatus(ctx, "running")
	if err != nil {
		log.Printf("[bg_jobs] recover running jobs failed: %v", err)
		return
	}
	for _, job := range jobs {
		job.Status = "interrupted"
		job.ExitCode = nil
		if err := m.db.UpdateBackgroundJob(ctx, job); err != nil {
			log.Printf("[bg_jobs] mark job %s interrupted failed: %v", job.ID, err)
		}
		log.Printf("[bg_jobs] recovered job %s: marked as interrupted", job.ID)
	}
}

// Start launches a background job and streams output to the topic.
// Returns the job ID and error.
func (m *BackgroundJobManager) Start(ctx context.Context, chatID, threadID int64, command string, cwd string) (string, error) {
	// Generate a unique job ID (8-character hex)
	jobID := generateJobID()

	// Parse the command: split by spaces, but respect quoted strings
	parts, err := parseCommand(command)
	if err != nil {
		return "", fmt.Errorf("parse command: %w", err)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}

	// Create the exec command
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Dir = cwd

	// Get stdout and stderr pipes
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("stderr pipe: %w", err)
	}

	// Create the job record
	job := &BackgroundJob{
		ID:        jobID,
		ChatID:    chatID,
		ThreadID:  threadID,
		Command:   command,
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}

	// Save to database
	if err := m.db.CreateBackgroundJob(ctx, job); err != nil {
		return "", fmt.Errorf("create job record: %w", err)
	}

	// Start the job in a background goroutine
	go m.runJob(ctx, job, cmd, stdout, stderr, jobID)

	return jobID, nil
}

// runJob executes a background job, streams output, and handles completion.
func (m *BackgroundJobManager) runJob(ctx context.Context, job *BackgroundJob, cmd *exec.Cmd, stdout, stderr interface{}, jobID string) {
	// Start the command
	if err := cmd.Start(); err != nil {
		log.Printf("[bg_jobs] job %s start failed: %v", job.ID, err)
		m.handleJobCompletion(ctx, job, nil, fmt.Sprintf("Failed to start: %v", err))
		return
	}

	// Track in memory AFTER Start() succeeds (prevents race on exec.Cmd struct)
	m.mu.Lock()
	m.jobs[jobID] = job
	m.cmds[jobID] = cmd
	m.mu.Unlock()

	// Send initial notification
	tidPtr := &job.ThreadID
	initMsg := fmt.Sprintf("⏳ Background job started: `%s`\n\nJob ID: `%s`", escapeCommand(job.Command), job.ID)
	if err := m.sender.SendResponse(ctx, job.ChatID, tidPtr, 0, initMsg); err != nil {
		log.Printf("[bg_jobs] job %s send start message failed: %v", job.ID, err)
	}

	// Stream output: combine stdout and stderr line-by-line
	outputLines := make(chan string, 100)
	done := make(chan struct{})

	// Use a WaitGroup to coordinate the two scanner goroutines
	// They own outputLines and will close it when both finish
	var scannersWg sync.WaitGroup
	scannersWg.Add(2)

	// Read stdout
	go func() {
		defer scannersWg.Done()
		scanner := bufio.NewScanner(stdout.(interface{ Read([]byte) (int, error) }))
		// Use a larger buffer to handle long lines (up to 1MB)
		buf := make([]byte, 0, 1024*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			outputLines <- scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[bg_jobs] job %s stdout scanner error: %v", job.ID, err)
		}
	}()

	// Read stderr
	go func() {
		defer scannersWg.Done()
		scanner := bufio.NewScanner(stderr.(interface{ Read([]byte) (int, error) }))
		// Use a larger buffer to handle long lines (up to 1MB)
		buf := make([]byte, 0, 1024*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			outputLines <- scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[bg_jobs] job %s stderr scanner error: %v", job.ID, err)
		}
	}()

	// Goroutine to close output channel when both scanners finish, then wait for process
	waitResult := make(chan error, 1)
	go func() {
		// Wait for both scanners to finish reading all output
		scannersWg.Wait()
		// Now it's safe to close the channel and call Wait
		close(outputLines)
		waitResult <- cmd.Wait()
		close(done)
	}()

	// Stream output with debouncing
	var (
		outputBuf    strings.Builder
		lastEdit     time.Time
		streamMsgID  int64
		lastLineSent string
	)

	// Process output lines
	streamingDone := false
	for !streamingDone {
		select {
		case line, ok := <-outputLines:
			if !ok {
				streamingDone = true
				break
			}
			// Add line to buffer
			if outputBuf.Len() > 0 {
				outputBuf.WriteString("\n")
			}
			outputBuf.WriteString(line)
			lastLineSent = line

			// Debounce: only send if enough time has passed
			if time.Since(lastEdit) >= jobStreamDebounce {
				text := outputBuf.String()
				if streamMsgID == 0 {
					// Send first message
					msgID, err := m.sender.sendInitialStream(ctx, job.ChatID, tidPtr, 0, text)
					if err != nil {
						log.Printf("[bg_jobs] job %s send output failed: %v", job.ID, err)
					} else {
						streamMsgID = msgID
					}
				} else {
					// Edit existing message
					if err := m.sender.EditMessage(ctx, job.ChatID, streamMsgID, text); err != nil {
						log.Printf("[bg_jobs] job %s edit output failed: %v", job.ID, err)
					}
				}
				lastEdit = time.Now()
			}

		case <-time.After(jobStreamDebounce):
			// Timeout: flush any pending output
			if outputBuf.Len() > 0 && lastLineSent != "" {
				text := outputBuf.String()
				if streamMsgID == 0 {
					msgID, err := m.sender.sendInitialStream(ctx, job.ChatID, tidPtr, 0, text)
					if err != nil {
						log.Printf("[bg_jobs] job %s send output failed: %v", job.ID, err)
					} else {
						streamMsgID = msgID
					}
				} else {
					if err := m.sender.EditMessage(ctx, job.ChatID, streamMsgID, text); err != nil {
						log.Printf("[bg_jobs] job %s edit output failed: %v", job.ID, err)
					}
				}
				lastEdit = time.Now()
			}

		case <-done:
			streamingDone = true
		}
	}

	// Get exit code from the goroutine that already called Wait()
	exitCode := 0
	select {
	case err := <-waitResult:
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
	default:
		// Process already reaped via the done channel path
	}

	// Final output flush
	finalText := outputBuf.String()
	if finalText != "" && streamMsgID != 0 {
		if err := m.sender.EditMessage(ctx, job.ChatID, streamMsgID, finalText); err != nil {
			log.Printf("[bg_jobs] job %s final edit failed: %v", job.ID, err)
		}
	}

	// Get last 20 lines for the completion message
	lines := strings.Split(finalText, "\n")
	var lastLines []string
	if len(lines) > 20 {
		lastLines = lines[len(lines)-20:]
	} else {
		lastLines = lines
	}

	// Build completion message
	var completionMsg strings.Builder
	if exitCode == 0 {
		completionMsg.WriteString(fmt.Sprintf("✓ Job done (exit 0)\n\nJob ID: `%s`", job.ID))
	} else {
		completionMsg.WriteString(fmt.Sprintf("✗ Job failed (exit %d)\n\nJob ID: `%s`", exitCode, job.ID))
	}

	if len(lastLines) > 0 {
		completionMsg.WriteString("\n\nLast output:\n")
		completionMsg.WriteString("```\n")
		for _, line := range lastLines {
			completionMsg.WriteString(line)
			completionMsg.WriteString("\n")
		}
		completionMsg.WriteString("```")
	}

	// Handle job completion
	m.handleJobCompletion(ctx, job, &exitCode, completionMsg.String())
}

// handleJobCompletion updates job state and sends completion notification.
func (m *BackgroundJobManager) handleJobCompletion(ctx context.Context, job *BackgroundJob, exitCode *int, message string) {
	// Update job status
	job.Status = "complete"
	if exitCode != nil && *exitCode != 0 {
		job.Status = "error"
	}
	job.ExitCode = exitCode

	// Update database
	if err := m.db.UpdateBackgroundJob(ctx, job); err != nil {
		log.Printf("[bg_jobs] update job %s failed: %v", job.ID, err)
	}

	// Remove from in-memory tracking
	m.mu.Lock()
	delete(m.jobs, job.ID)
	delete(m.cmds, job.ID)
	m.mu.Unlock()

	// Send completion notification
	tidPtr := &job.ThreadID
	if err := m.sender.SendResponse(ctx, job.ChatID, tidPtr, 0, message); err != nil {
		log.Printf("[bg_jobs] job %s send completion failed: %v", job.ID, err)
	}

	log.Printf("[bg_jobs] job %s completed: status=%s exit=%v", job.ID, job.Status, exitCode)
}

// Kill sends SIGTERM to a running job.
func (m *BackgroundJobManager) Kill(ctx context.Context, jobID string) error {
	m.mu.Lock()
	job, jobOk := m.jobs[jobID]
	cmd, cmdOk := m.cmds[jobID]

	// Capture the fields we need while holding the lock
	var process *os.Process
	var chatID int64
	var threadID int64
	var jobCopy BackgroundJob

	if cmdOk && cmd != nil && cmd.Process != nil {
		process = cmd.Process
	}
	if jobOk && job != nil {
		chatID = job.ChatID
		threadID = job.ThreadID
		jobCopy = *job // Copy the job struct
		job.Status = "interrupted"
	}

	// Remove from in-memory tracking while holding the lock
	if jobOk {
		delete(m.jobs, jobID)
	}
	if cmdOk {
		delete(m.cmds, jobID)
	}
	m.mu.Unlock()

	if !jobOk {
		return fmt.Errorf("job %s not found or not running", jobID)
	}

	if process == nil {
		return fmt.Errorf("job %s has no active process", jobID)
	}

	// Send SIGTERM (using captured process reference, no lock needed)
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("send signal to job %s: %w", jobID, err)
	}

	// Update database
	jobCopy.Status = "interrupted"
	if err := m.db.UpdateBackgroundJob(ctx, &jobCopy); err != nil {
		log.Printf("[bg_jobs] update job %s after kill failed: %v", jobID, err)
	}

	// Send notification
	tidPtr := &threadID
	msg := fmt.Sprintf("⚠️ Job `%s` killed", jobID)
	if err := m.sender.SendResponse(ctx, chatID, tidPtr, 0, msg); err != nil {
		log.Printf("[bg_jobs] job %s send kill notification failed: %v", jobID, err)
	}

	log.Printf("[bg_jobs] job %s killed", jobID)
	return nil
}

// List returns all background jobs for a given chat/thread.
func (m *BackgroundJobManager) List(ctx context.Context, chatID, threadID int64) ([]*BackgroundJob, error) {
	// Get jobs from database
	jobs, err := m.db.ListBackgroundJobsForTopic(ctx, chatID, threadID)
	if err != nil {
		return nil, err
	}

	// Enhance with in-memory status for running jobs
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, job := range jobs {
		if _, ok := m.cmds[job.ID]; ok {
			// Job is still running
			job.Status = "running"
		}
	}

	return jobs, nil
}

// generateJobID generates a unique 8-character hex job ID.
func generateJobID() string {
	return fmt.Sprintf("%08x", uint32(time.Now().UnixNano()))
}

// parseCommand parses a command string into parts, respecting quoted strings.
func parseCommand(cmd string) ([]string, error) {
	var parts []string
	var current strings.Builder
	inQuotes := false
	escapeNext := false

	for _, r := range cmd {
		switch {
		case escapeNext:
			current.WriteRune(r)
			escapeNext = false
		case r == '\\':
			escapeNext = true
		case r == '"':
			inQuotes = !inQuotes
		case r == ' ' && !inQuotes:
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}

	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	if inQuotes {
		return nil, fmt.Errorf("unclosed quotes")
	}

	return parts, nil
}

// escapeCommand escapes a command string for safe markdown display.
func escapeCommand(cmd string) string {
	return strings.ReplaceAll(cmd, "`", "\\`")
}
