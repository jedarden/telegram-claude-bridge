package bridge

import (
	"context"
	"os/exec"
)

// command is an interface for a running command, allowing test mocks.
type command interface {
	CombinedOutput() ([]byte, error)
	Run() error
}

// commandExec is an interface for executing commands, allowing test mocks.
type commandExec interface {
	CommandContext(ctx context.Context, name string, args ...string) command
}

// realCommandExec implements commandExec using os/exec.Command.
type realCommandExec struct{}

func (r realCommandExec) CommandContext(ctx context.Context, name string, args ...string) command {
	return &realCommand{cmd: exec.CommandContext(ctx, name, args...)}
}

// realCommand wraps exec.Cmd to implement the command interface.
type realCommand struct {
	cmd *exec.Cmd
}

func (r *realCommand) CombinedOutput() ([]byte, error) {
	return r.cmd.CombinedOutput()
}

func (r *realCommand) Run() error {
	return r.cmd.Run()
}
