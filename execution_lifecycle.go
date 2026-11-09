package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

const executionWaitDelay = 250 * time.Millisecond
const executionShutdownTimeout = 2 * time.Second

// Commands own a process group so cancellation also reaches descendants that
// inherited an output pipe. WaitDelay bounds pipe copying after parent exit.
func executionCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = executionWaitDelay
	if runtime.GOOS == "linux" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return killExecutionGroup(cmd) }
	}
	return cmd
}

func killExecutionGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if runtime.GOOS != "linux" {
		return cmd.Process.Kill()
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func waitExecution(cmd *exec.Cmd) error {
	err := cmd.Wait()
	// Parent exit does not prove its descendants exited. This also cleans up
	// inherited-pipe holders after WaitDelay has closed the parent's pipe ends.
	_ = killExecutionGroup(cmd)
	return err
}

func runExecution(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return waitExecution(cmd)
}
