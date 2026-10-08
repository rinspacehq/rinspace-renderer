package nodeworker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const ContractVersion = "rin-node-worker/v1"

var (
	ErrClosed           = errors.New("node worker supervisor is closed")
	ErrRequestTooLarge  = errors.New("node worker request exceeds byte limit")
	ErrResponseTooLarge = errors.New("node worker response exceeds byte limit")
)

type Config struct {
	Command          string
	Args             []string
	Dir              string
	Environment      map[string]string
	Workers          int
	MaxRequestBytes  int64
	MaxResponseBytes int64
	MaxTasks         uint64
	MaxRSSBytes      uint64
	StartTimeout     time.Duration
	StopGrace        time.Duration
}

type Snapshot struct {
	Capacity       int
	Alive          int64
	Active         int64
	Starts         uint64
	Restarts       uint64
	Crashes        uint64
	Cancellations  uint64
	ProtocolErrors uint64
	Recycles       uint64
	Tasks          uint64
	RSSBytes       uint64
}

type RemoteError struct {
	Code    string
	Message string
}

func (err *RemoteError) Error() string {
	if err == nil {
		return ""
	}
	if err.Code == "" {
		return err.Message
	}
	if err.Message == "" {
		return err.Code
	}
	return err.Code + ": " + err.Message
}

type requestEnvelope struct {
	ContractVersion string          `json:"contractVersion"`
	ID              string          `json:"id"`
	Operation       string          `json:"operation"`
	DeadlineUnixMS  int64           `json:"deadlineUnixMs,omitempty"`
	Payload         json.RawMessage `json:"payload"`
}

type responseEnvelope struct {
	ContractVersion string          `json:"contractVersion"`
	ID              string          `json:"id"`
	OK              bool            `json:"ok"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           *RemoteError    `json:"error,omitempty"`
	Metrics         workerMetrics   `json:"metrics,omitempty"`
	Recycle         bool            `json:"recycle,omitempty"`
}

type workerMetrics struct {
	Tasks    uint64 `json:"tasks,omitempty"`
	RSSBytes uint64 `json:"rssBytes,omitempty"`
}

type Supervisor struct {
	cfg       Config
	startMu   sync.Mutex
	stateMu   sync.Mutex
	started   bool
	closed    bool
	stop      chan struct{}
	done      sync.WaitGroup
	tempDir   string
	slots     []*slot
	available chan *slot

	alive          atomic.Int64
	active         atomic.Int64
	starts         atomic.Uint64
	restarts       atomic.Uint64
	crashes        atomic.Uint64
	cancellations  atomic.Uint64
	protocolErrors atomic.Uint64
	recycles       atomic.Uint64
	tasks          atomic.Uint64
	rssBytes       atomic.Uint64
}

type slot struct {
	worker *process
}

type process struct {
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       *bufio.Reader
	stderr       *boundedBuffer
	done         chan struct{}
	waitText     atomic.Value
	dead         atomic.Bool
	stopping     atomic.Bool
	crashCounted atomic.Bool
	tasks        uint64
	rssBytes     uint64
	tempDir      string
}

func New(config Config) (*Supervisor, error) {
	config.Command = strings.TrimSpace(config.Command)
	if config.Command == "" {
		return nil, errors.New("node worker command is empty")
	}
	if config.Workers <= 0 {
		return nil, errors.New("node worker count must be positive")
	}
	if config.MaxRequestBytes <= 0 || config.MaxResponseBytes <= 0 {
		return nil, errors.New("node worker byte limits must be positive")
	}
	if config.StartTimeout <= 0 {
		config.StartTimeout = 10 * time.Second
	}
	if config.StopGrace <= 0 {
		config.StopGrace = 500 * time.Millisecond
	}
	supervisor := &Supervisor{
		cfg: config, stop: make(chan struct{}),
		available: make(chan *slot, config.Workers),
	}
	return supervisor, nil
}

func (supervisor *Supervisor) Do(ctx context.Context, operation string, payload any, result any) error {
	if err := supervisor.begin(); err != nil {
		return err
	}
	defer supervisor.done.Done()
	if err := ctx.Err(); err != nil {
		return err
	}
	operation = strings.TrimSpace(operation)
	if !validOperation(operation) {
		return errors.New("node worker operation is invalid")
	}
	payloadBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal node worker payload: %w", err)
	}
	id, err := requestID()
	if err != nil {
		return fmt.Errorf("create node worker request id: %w", err)
	}
	envelope := requestEnvelope{ContractVersion: ContractVersion, ID: id, Operation: operation, Payload: payloadBody}
	if deadline, ok := ctx.Deadline(); ok {
		envelope.DeadlineUnixMS = deadline.UnixMilli()
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal node worker request: %w", err)
	}
	if int64(len(body)+1) > supervisor.cfg.MaxRequestBytes {
		return ErrRequestTooLarge
	}
	if err := supervisor.ensureStarted(); err != nil {
		return err
	}

	var current *slot
	select {
	case current = <-supervisor.available:
	case <-ctx.Done():
		return ctx.Err()
	case <-supervisor.stop:
		return ErrClosed
	}
	supervisor.active.Add(1)
	defer func() {
		supervisor.active.Add(-1)
		supervisor.available <- current
	}()

	if current.worker == nil {
		current.worker, err = supervisor.startProcess()
		if err != nil {
			return fmt.Errorf("start node worker: %w", err)
		}
		supervisor.restarts.Add(1)
	}
	response, exchangeErr := supervisor.exchange(ctx, current.worker, append(body, '\n'))
	recycle := exchangeErr != nil
	if exchangeErr == nil {
		current.worker.tasks++
		current.worker.rssBytes = response.Metrics.RSSBytes
		supervisor.tasks.Add(1)
		supervisor.rssBytes.Store(response.Metrics.RSSBytes)
		recycle = response.Recycle ||
			(supervisor.cfg.MaxTasks > 0 && current.worker.tasks >= supervisor.cfg.MaxTasks) ||
			(supervisor.cfg.MaxRSSBytes > 0 && response.Metrics.RSSBytes >= supervisor.cfg.MaxRSSBytes)
		if !response.OK && response.Error == nil {
			exchangeErr = errors.New("node worker error response is missing error details")
		} else if response.OK && result != nil && len(response.Result) == 0 {
			exchangeErr = errors.New("node worker returned an empty result")
		} else if response.OK && result != nil {
			if err := json.Unmarshal(response.Result, result); err != nil {
				exchangeErr = fmt.Errorf("decode node worker result: %w", err)
			}
		}
		if exchangeErr != nil {
			supervisor.protocolErrors.Add(1)
			recycle = true
		}
	}
	if recycle {
		if exchangeErr == nil {
			supervisor.recycles.Add(1)
		}
		supervisor.replace(current)
	}
	if exchangeErr != nil {
		return exchangeErr
	}
	if !response.OK {
		return response.Error
	}
	return nil
}

func (supervisor *Supervisor) Ready(ctx context.Context) error {
	if err := supervisor.begin(); err != nil {
		return err
	}
	defer supervisor.done.Done()
	if err := supervisor.ensureStarted(); err != nil {
		return fmt.Errorf("worker_unavailable: %w", err)
	}
	supervisor.repairAvailableWorkers()
	if supervisor.alive.Load() < int64(supervisor.cfg.Workers) {
		return fmt.Errorf("worker_unavailable: %d/%d workers alive", supervisor.alive.Load(), supervisor.cfg.Workers)
	}
	// Saturation is queue state, not a liveness failure. Avoid making status and
	// capabilities wait behind a long render when every healthy slot is leased.
	if supervisor.active.Load() >= int64(supervisor.cfg.Workers) {
		return nil
	}
	var response map[string]any
	if err := supervisor.Do(ctx, "health", map[string]any{}, &response); err != nil {
		return fmt.Errorf("worker_unavailable: %w", err)
	}
	if ready, _ := response["ready"].(bool); !ready {
		return errors.New("worker_unavailable: health response is not ready")
	}
	return nil
}

func (supervisor *Supervisor) repairAvailableWorkers() {
	available := make([]*slot, 0, supervisor.cfg.Workers)
	for index := 0; index < supervisor.cfg.Workers; index++ {
		select {
		case current := <-supervisor.available:
			available = append(available, current)
		default:
			index = supervisor.cfg.Workers
		}
	}
	for _, current := range available {
		if current.worker == nil || processDone(current.worker) {
			supervisor.replace(current)
		}
		supervisor.available <- current
	}
}

func processDone(current *process) bool {
	if current == nil {
		return true
	}
	select {
	case <-current.done:
		return true
	default:
		return false
	}
}

func (supervisor *Supervisor) Snapshot() Snapshot {
	return Snapshot{
		Capacity: supervisor.cfg.Workers, Alive: supervisor.alive.Load(), Active: supervisor.active.Load(),
		Starts: supervisor.starts.Load(), Restarts: supervisor.restarts.Load(), Crashes: supervisor.crashes.Load(),
		Cancellations: supervisor.cancellations.Load(), ProtocolErrors: supervisor.protocolErrors.Load(),
		Recycles: supervisor.recycles.Load(), Tasks: supervisor.tasks.Load(), RSSBytes: supervisor.rssBytes.Load(),
	}
}

func (supervisor *Supervisor) Close() error {
	supervisor.stateMu.Lock()
	if supervisor.closed {
		supervisor.stateMu.Unlock()
		return nil
	}
	supervisor.closed = true
	close(supervisor.stop)
	supervisor.stateMu.Unlock()
	supervisor.done.Wait()
	for range supervisor.slots {
		current := <-supervisor.available
		if current.worker != nil {
			supervisor.stopProcess(current.worker)
			current.worker = nil
		}
	}
	if supervisor.tempDir != "" {
		return os.RemoveAll(supervisor.tempDir)
	}
	return nil
}

func (supervisor *Supervisor) begin() error {
	supervisor.stateMu.Lock()
	defer supervisor.stateMu.Unlock()
	if supervisor.closed {
		return ErrClosed
	}
	supervisor.done.Add(1)
	return nil
}

func (supervisor *Supervisor) ensureStarted() error {
	supervisor.startMu.Lock()
	defer supervisor.startMu.Unlock()
	if supervisor.started {
		return nil
	}
	tempDir, err := os.MkdirTemp("", "rin-node-worker-")
	if err != nil {
		return fmt.Errorf("create node worker temp dir: %w", err)
	}
	supervisor.tempDir = tempDir
	var firstErr error
	for index := 0; index < supervisor.cfg.Workers; index++ {
		current := &slot{}
		current.worker, err = supervisor.startProcess()
		if err != nil && firstErr == nil {
			firstErr = err
		}
		supervisor.slots = append(supervisor.slots, current)
		supervisor.available <- current
	}
	supervisor.started = true
	if supervisor.alive.Load() == 0 && firstErr != nil {
		return fmt.Errorf("start node workers: %w", firstErr)
	}
	return nil
}

func (supervisor *Supervisor) startProcess() (*process, error) {
	ctx, cancel := context.WithTimeout(context.Background(), supervisor.cfg.StartTimeout)
	defer cancel()
	commandPath, err := exec.LookPath(supervisor.cfg.Command)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(commandPath, supervisor.cfg.Args...)
	cmd.Dir = supervisor.cfg.Dir
	workerTempDir, err := os.MkdirTemp(supervisor.tempDir, "worker-")
	if err != nil {
		return nil, err
	}
	cmd.Env = supervisor.environment(workerTempDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		os.RemoveAll(workerTempDir)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		os.RemoveAll(workerTempDir)
		return nil, err
	}
	stderr := newBoundedBuffer(32 << 10)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		stdin.Close()
		os.RemoveAll(workerTempDir)
		return nil, err
	}
	current := &process{cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 64<<10), stderr: stderr, done: make(chan struct{}), tempDir: workerTempDir}
	go func() {
		waitErr := cmd.Wait()
		if waitErr != nil {
			current.waitText.Store(waitErr.Error())
		}
		if !current.stopping.Load() {
			supervisor.markCrash(current)
		}
		if current.dead.CompareAndSwap(false, true) {
			supervisor.alive.Add(-1)
		}
		close(current.done)
	}()
	select {
	case <-ctx.Done():
		supervisor.stopProcess(current)
		return nil, ctx.Err()
	default:
	}
	supervisor.alive.Add(1)
	supervisor.starts.Add(1)
	return current, nil
}

func (supervisor *Supervisor) exchange(ctx context.Context, current *process, body []byte) (responseEnvelope, error) {
	type outcome struct {
		response responseEnvelope
		err      error
	}
	completed := make(chan outcome, 1)
	go func() {
		if _, err := current.stdin.Write(body); err != nil {
			completed <- outcome{err: fmt.Errorf("write node worker request: %w", err)}
			return
		}
		line, err := readBoundedLine(current.stdout, supervisor.cfg.MaxResponseBytes)
		if err != nil {
			completed <- outcome{err: err}
			return
		}
		var response responseEnvelope
		if err := json.Unmarshal(line, &response); err != nil {
			completed <- outcome{err: fmt.Errorf("decode node worker response: %w", err)}
			return
		}
		completed <- outcome{response: response}
	}()
	select {
	case outcome := <-completed:
		if outcome.err != nil {
			if errors.Is(outcome.err, ErrResponseTooLarge) || strings.Contains(outcome.err.Error(), "decode node worker response") {
				supervisor.protocolErrors.Add(1)
			} else {
				supervisor.markCrash(current)
			}
			return responseEnvelope{}, supervisor.workerError(current, outcome.err)
		}
		if outcome.response.ContractVersion != ContractVersion || outcome.response.ID != extractRequestID(body) {
			supervisor.protocolErrors.Add(1)
			return responseEnvelope{}, errors.New("node worker response correlation failed")
		}
		return outcome.response, nil
	case <-ctx.Done():
		supervisor.cancellations.Add(1)
		supervisor.stopProcess(current)
		<-completed
		return responseEnvelope{}, ctx.Err()
	case <-supervisor.stop:
		supervisor.stopProcess(current)
		<-completed
		return responseEnvelope{}, ErrClosed
	}
}

func (supervisor *Supervisor) replace(current *slot) {
	if current.worker != nil {
		supervisor.stopProcess(current.worker)
		current.worker = nil
	}
	supervisor.stateMu.Lock()
	closed := supervisor.closed
	supervisor.stateMu.Unlock()
	if closed {
		return
	}
	worker, err := supervisor.startProcess()
	if err == nil {
		current.worker = worker
		supervisor.restarts.Add(1)
	}
}

func (supervisor *Supervisor) stopProcess(current *process) {
	if current == nil || current.cmd == nil || current.cmd.Process == nil {
		return
	}
	current.stopping.Store(true)
	current.stdin.Close()
	select {
	case <-current.done:
	default:
		_ = syscall.Kill(-current.cmd.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(supervisor.cfg.StopGrace)
		select {
		case <-current.done:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			_ = syscall.Kill(-current.cmd.Process.Pid, syscall.SIGKILL)
			<-current.done
		}
	}
	_ = os.RemoveAll(current.tempDir)
}

func (supervisor *Supervisor) markCrash(current *process) {
	if current != nil && current.crashCounted.CompareAndSwap(false, true) {
		supervisor.crashes.Add(1)
	}
}

func (supervisor *Supervisor) workerError(current *process, cause error) error {
	message := strings.TrimSpace(current.stderr.String())
	if message == "" {
		message = cause.Error()
	}
	if len(message) > 512 {
		message = message[len(message)-512:]
	}
	return fmt.Errorf("node worker failed: %s: %w", message, cause)
}

func (supervisor *Supervisor) environment(workerTempDir string) []string {
	values := map[string]string{
		"PATH": os.Getenv("PATH"), "LANG": firstNonEmpty(os.Getenv("LANG"), "C.UTF-8"),
		"TZ": firstNonEmpty(os.Getenv("TZ"), "UTC"), "HOME": workerTempDir, "TMPDIR": workerTempDir,
	}
	for key, value := range supervisor.cfg.Environment {
		if validEnvironmentKey(key) {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	// Stable ordering is useful in diagnostics and tests.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func readBoundedLine(reader *bufio.Reader, limit int64) ([]byte, error) {
	var body []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if int64(len(body)+len(fragment)) > limit {
			return nil, ErrResponseTooLarge
		}
		body = append(body, fragment...)
		if err == nil {
			return bytes.TrimSuffix(body, []byte{'\n'}), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func requestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func extractRequestID(body []byte) string {
	var request struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &request)
	return request.ID
}

func validOperation(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func validEnvironmentKey(value string) bool {
	if value == "" || strings.Contains(value, "=") {
		return false
	}
	for _, character := range value {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	body  []byte
}

func newBoundedBuffer(limit int) *boundedBuffer { return &boundedBuffer{limit: limit} }

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.body = append(buffer.body, value...)
	if len(buffer.body) > buffer.limit {
		buffer.body = append([]byte(nil), buffer.body[len(buffer.body)-buffer.limit:]...)
	}
	return len(value), nil
}

func (buffer *boundedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.body)
}

func ScriptDir(scriptPath string) string {
	absolute, err := filepath.Abs(scriptPath)
	if err != nil {
		return filepath.Dir(scriptPath)
	}
	return filepath.Dir(absolute)
}
