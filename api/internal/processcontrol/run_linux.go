package processcontrol

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const DefaultGrace = 10 * time.Second

// Run starts cmd in its own process group. Cancellation first asks the complete child tree to
// terminate, then kills the group after grace. The caller still observes ctx.Err(), while Wait is
// always reaped before Run returns.
func Run(ctx context.Context, cmd *exec.Cmd, grace time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if grace <= 0 {
		grace = DefaultGrace
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		return err
	case <-ctx.Done():
	}

	signalGroup(cmd.Process, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-waited:
		return ctx.Err()
	case <-timer.C:
		signalGroup(cmd.Process, syscall.SIGKILL)
		<-waited
		return ctx.Err()
	}
}

func signalGroup(process *os.Process, signal syscall.Signal) {
	if process == nil {
		return
	}
	if err := syscall.Kill(-process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = process.Signal(signal)
	}
}
