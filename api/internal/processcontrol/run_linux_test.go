package processcontrol

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestRunTerminatesCooperativeProcessGroup(t *testing.T) {
	assertCanceledWithin(t, `trap 'exit 0' TERM; echo ready; while :; do sleep 1; done`, time.Second)
}

func TestRunKillsProcessGroupAfterGrace(t *testing.T) {
	assertCanceledWithin(t, `trap '' TERM; echo ready; while :; do sleep 1; done`, 50*time.Millisecond)
}

func assertCanceledWithin(t *testing.T, script string, grace time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command("sh", "-c", script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- Run(ctx, cmd, grace) }()
	if !bufio.NewScanner(stdout).Scan() {
		t.Fatal("child did not become ready")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v", err)
		}
		if elapsed := time.Since(started); elapsed > grace+2*time.Second {
			t.Fatalf("cancellation took %v with grace %v", elapsed, grace)
		}
	case <-time.After(grace + 3*time.Second):
		t.Fatal("child process group was not reaped")
	}
}
