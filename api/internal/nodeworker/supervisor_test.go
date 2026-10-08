package nodeworker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorCorrelatesRequestsAndRestrictsEnvironment(t *testing.T) {
	t.Setenv("RIN_PARENT_SECRET", "must-not-leak")
	supervisor := newTestSupervisor(t, Config{Workers: 1})
	defer supervisor.Close()
	var result struct {
		Value  string `json:"value"`
		Secret string `json:"secret"`
	}
	if err := supervisor.Do(context.Background(), "echo", map[string]any{"value": "hello"}, &result); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if result.Value != "hello" || result.Secret != "" {
		t.Fatalf("unexpected result or leaked environment: %#v", result)
	}
	if snapshot := supervisor.Snapshot(); snapshot.Alive != 1 || snapshot.Tasks != 1 {
		t.Fatalf("unexpected supervisor snapshot: %#v", snapshot)
	}
}

func TestSupervisorRejectsOversizedRequestBeforeStartingWorker(t *testing.T) {
	supervisor := newTestSupervisor(t, Config{Workers: 1, MaxRequestBytes: 256})
	defer supervisor.Close()
	err := supervisor.Do(context.Background(), "echo", map[string]any{"value": strings.Repeat("x", 1024)}, nil)
	if !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("expected request limit error, got %v", err)
	}
	if supervisor.Snapshot().Starts != 0 {
		t.Fatalf("request rejected before admission should not start workers: %#v", supervisor.Snapshot())
	}
}

func TestSupervisorReplacesWorkerAfterOversizedOrMismatchedResponse(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		want      error
	}{
		{name: "oversized", operation: "large", want: ErrResponseTooLarge},
		{name: "correlation", operation: "mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			supervisor := newTestSupervisor(t, Config{Workers: 1, MaxResponseBytes: 512})
			defer supervisor.Close()
			err := supervisor.Do(context.Background(), test.operation, map[string]any{}, nil)
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("expected %v, got %v", test.want, err)
			}
			if test.want == nil && (err == nil || !strings.Contains(err.Error(), "correlation")) {
				t.Fatalf("expected correlation error, got %v", err)
			}
			var result map[string]any
			if err := supervisor.Do(context.Background(), "echo", map[string]any{"value": "after"}, &result); err != nil {
				t.Fatalf("replacement worker did not serve next request: %v", err)
			}
			snapshot := supervisor.Snapshot()
			if snapshot.Restarts < 1 || snapshot.ProtocolErrors < 1 || snapshot.Alive != 1 {
				t.Fatalf("expected healthy replacement and protocol accounting: %#v", snapshot)
			}
		})
	}
}

func TestSupervisorCancellationOnlyFailsCurrentLeaseAndRestarts(t *testing.T) {
	supervisor := newTestSupervisor(t, Config{Workers: 1})
	defer supervisor.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- supervisor.Do(ctx, "sleep", map[string]any{"milliseconds": 500}, nil)
	}()
	waitForActiveWorker(t, supervisor)
	secondDone := make(chan error, 1)
	go func() {
		var result map[string]any
		secondDone <- supervisor.Do(context.Background(), "echo", map[string]any{"value": "queued"}, &result)
	}()
	if err := <-firstDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error for current lease, got %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("unrelated queued request should run on replacement: %v", err)
	}
	snapshot := supervisor.Snapshot()
	if snapshot.Cancellations != 1 || snapshot.Restarts < 1 || snapshot.Alive != 1 {
		t.Fatalf("unexpected cancellation/restart snapshot: %#v", snapshot)
	}
}

func TestSupervisorCancellationTerminatesWorkerProcessGroup(t *testing.T) {
	supervisor := newTestSupervisor(t, Config{Workers: 1})
	defer supervisor.Close()
	pidPath := t.TempDir() + "/child.pid"
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := supervisor.Do(ctx, "child_sleep", map[string]any{"path": pidPath}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline while child process was running, got %v", err)
	}
	body, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatalf("read helper child pid: %v", readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
	if parseErr != nil {
		t.Fatalf("parse helper child pid: %v", parseErr)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d survived worker process-group cancellation", pid)
}

func TestSupervisorRestartsAfterCrash(t *testing.T) {
	supervisor := newTestSupervisor(t, Config{Workers: 1})
	defer supervisor.Close()
	if err := supervisor.Do(context.Background(), "crash", map[string]any{}, nil); err == nil {
		t.Fatal("expected worker crash")
	}
	var result map[string]any
	if err := supervisor.Do(context.Background(), "echo", map[string]any{"value": "recovered"}, &result); err != nil {
		t.Fatalf("replacement after crash: %v", err)
	}
	if snapshot := supervisor.Snapshot(); snapshot.Crashes < 1 || snapshot.Restarts < 1 || snapshot.Alive != 1 {
		t.Fatalf("unexpected crash snapshot: %#v", snapshot)
	}
}

func TestSupervisorHealthRepairsWorkerThatCrashesWhileIdle(t *testing.T) {
	supervisor := newTestSupervisor(t, Config{Workers: 1})
	defer supervisor.Close()
	var first struct {
		PID int `json:"pid"`
	}
	if err := supervisor.Do(context.Background(), "exit_after", map[string]any{}, &first); err != nil {
		t.Fatalf("request before idle crash: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for supervisor.Snapshot().Alive != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if supervisor.Snapshot().Alive != 0 {
		t.Fatalf("helper did not exit after response: %#v", supervisor.Snapshot())
	}
	if err := supervisor.Ready(context.Background()); err != nil {
		t.Fatalf("health should repair idle worker: %v", err)
	}
	var second struct {
		PID int `json:"pid"`
	}
	if err := supervisor.Do(context.Background(), "echo", map[string]any{}, &second); err != nil {
		t.Fatalf("request after health repair: %v", err)
	}
	if first.PID == second.PID || supervisor.Snapshot().Restarts < 1 {
		t.Fatalf("expected idle worker replacement: first=%d second=%d snapshot=%#v", first.PID, second.PID, supervisor.Snapshot())
	}
}

func TestSupervisorRecyclesByTaskAndMemoryPolicy(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
		rss    uint64
	}{
		{name: "task", config: Config{Workers: 1, MaxTasks: 1}},
		{name: "memory", config: Config{Workers: 1, MaxRSSBytes: 64}, rss: 128},
	} {
		t.Run(test.name, func(t *testing.T) {
			supervisor := newTestSupervisor(t, test.config)
			defer supervisor.Close()
			var first, second struct {
				PID int `json:"pid"`
			}
			if err := supervisor.Do(context.Background(), "echo", map[string]any{"rssBytes": test.rss}, &first); err != nil {
				t.Fatalf("first request: %v", err)
			}
			if err := supervisor.Do(context.Background(), "echo", map[string]any{}, &second); err != nil {
				t.Fatalf("second request: %v", err)
			}
			if first.PID == second.PID {
				t.Fatalf("expected recycled process, both requests used pid %d", first.PID)
			}
			if snapshot := supervisor.Snapshot(); snapshot.Recycles < 1 || snapshot.Restarts < 1 {
				t.Fatalf("expected recycle accounting: %#v", snapshot)
			}
		})
	}
}

func TestSupervisorHealthAndClose(t *testing.T) {
	supervisor := newTestSupervisor(t, Config{Workers: 2})
	if err := supervisor.Ready(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if snapshot := supervisor.Snapshot(); snapshot.Alive != 2 || snapshot.Capacity != 2 {
		t.Fatalf("unexpected healthy snapshot: %#v", snapshot)
	}
	if err := supervisor.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := supervisor.Do(context.Background(), "echo", map[string]any{}, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected closed error, got %v", err)
	}
}

func TestSupervisorReadinessRemainsResponsiveWhilePoolIsSaturated(t *testing.T) {
	supervisor := newTestSupervisor(t, Config{Workers: 1})
	defer supervisor.Close()
	renderDone := make(chan error, 1)
	go func() {
		renderDone <- supervisor.Do(context.Background(), "sleep", map[string]any{"milliseconds": 100}, nil)
	}()
	waitForActiveWorker(t, supervisor)
	started := time.Now()
	if err := supervisor.Ready(context.Background()); err != nil {
		t.Fatalf("saturated healthy pool should remain ready: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("readiness waited behind saturated worker for %s", elapsed)
	}
	if err := <-renderDone; err != nil {
		t.Fatalf("render failed after readiness check: %v", err)
	}
}

func newTestSupervisor(t *testing.T, override Config) *Supervisor {
	t.Helper()
	config := Config{
		Command: os.Args[0], Args: []string{"-test.run=TestNodeWorkerHelper"},
		Environment: map[string]string{"RIN_NODEWORKER_HELPER": "1"},
		Workers:     1, MaxRequestBytes: 64 << 10, MaxResponseBytes: 64 << 10,
		StartTimeout: time.Second, StopGrace: 50 * time.Millisecond,
	}
	if override.Workers != 0 {
		config.Workers = override.Workers
	}
	if override.MaxRequestBytes != 0 {
		config.MaxRequestBytes = override.MaxRequestBytes
	}
	if override.MaxResponseBytes != 0 {
		config.MaxResponseBytes = override.MaxResponseBytes
	}
	config.MaxTasks = override.MaxTasks
	config.MaxRSSBytes = override.MaxRSSBytes
	supervisor, err := New(config)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	return supervisor
}

func waitForActiveWorker(t *testing.T, supervisor *Supervisor) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if supervisor.Snapshot().Active == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("worker did not become active")
}

func TestNodeWorkerHelper(t *testing.T) {
	if os.Getenv("RIN_NODEWORKER_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1<<20)
	var tasks uint64
	for scanner.Scan() {
		var request requestEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		if request.Operation == "crash" {
			fmt.Fprintln(os.Stderr, "intentional crash")
			os.Exit(9)
		}
		var payload struct {
			Value        string `json:"value"`
			Milliseconds int    `json:"milliseconds"`
			RSSBytes     uint64 `json:"rssBytes"`
			Path         string `json:"path"`
		}
		_ = json.Unmarshal(request.Payload, &payload)
		if request.Operation == "sleep" {
			time.Sleep(time.Duration(payload.Milliseconds) * time.Millisecond)
		}
		if request.Operation == "child_sleep" {
			child := exec.Command("sh", "-c", "sleep 30")
			if err := child.Start(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(5)
			}
			if err := os.WriteFile(payload.Path, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(6)
			}
			_ = child.Wait()
		}
		tasks++
		response := responseEnvelope{
			ContractVersion: ContractVersion, ID: request.ID, OK: true,
			Metrics: workerMetrics{Tasks: tasks, RSSBytes: payload.RSSBytes},
		}
		switch request.Operation {
		case "health":
			response.Result = json.RawMessage(`{"ready":true}`)
		case "large":
			body, _ := json.Marshal(map[string]string{"value": strings.Repeat("x", 2048)})
			response.Result = body
		case "mismatch":
			response.ID = "wrong-request-id"
			response.Result = json.RawMessage(`{}`)
		default:
			body, _ := json.Marshal(map[string]any{
				"value": payload.Value, "pid": os.Getpid(), "secret": os.Getenv("RIN_PARENT_SECRET"),
			})
			response.Result = body
		}
		body, _ := json.Marshal(response)
		fmt.Println(string(body))
		if request.Operation == "exit_after" {
			os.Exit(8)
		}
	}
	os.Exit(0)
}
