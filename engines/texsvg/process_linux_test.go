package main

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestRunProcessKillsAfterGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command("sh", "-c", `trap '' TERM; echo ready; while :; do sleep 1; done`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runProcess(ctx, cmd, 50*time.Millisecond) }()
	if !bufio.NewScanner(stdout).Scan() {
		t.Fatal("child did not become ready")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runProcess() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TeX child process group was not killed")
	}
}
