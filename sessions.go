package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

type boundedBuffer struct {
	mu        sync.Mutex
	b         bytes.Buffer
	limit     int
	consumed  int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	left := b.limit - b.b.Len()
	if left > 0 {
		if left > n {
			left = n
		}
		_, _ = b.b.Write(p[:left])
	}
	if left < n {
		b.truncated = true
	}
	return n, nil
}
func (b *boundedBuffer) Snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String(), b.truncated
}

// CodexFree drains resident-command output on each poll. Keeping the bounded
// backing buffer but returning only new text avoids filling model context
// with the same output after every write_stdin.
func (b *boundedBuffer) Drain() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw := b.b.Bytes()
	if b.consumed > len(raw) {
		b.consumed = len(raw)
	}
	text := string(raw[b.consumed:])
	b.consumed = len(raw)
	return text, b.truncated
}

type Session struct {
	ID           int64
	owner        string
	started      time.Time
	mu           sync.Mutex
	command      *exec.Cmd
	stdin        *os.File
	inputGate    chan struct{}
	cancel       context.CancelFunc
	output       *boundedBuffer
	done         chan struct{}
	exitCode     int
	errorMessage string
	finished     time.Time
}

type SessionManager struct {
	mu       sync.Mutex
	sessions map[int64]*Session
	stopping bool
}
type CommandInput struct {
	Cmd             string `json:"cmd" jsonschema:"POSIX shell command executed as the agent OS account."`
	Workdir         string `json:"workdir,omitempty" jsonschema:"Working directory; default to config work_root."`
	YieldTimeMS     int    `json:"yield_time_ms,omitempty" jsonschema:"Wait 250-30000ms, then return a session ID if still running."`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty" jsonschema:"Override timeout in seconds, maximum configured timeout."`
	Shell           string `json:"shell,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	TTY             bool   `json:"tty,omitempty"`
	Scope           string `json:"scope,omitempty" jsonschema:"Stable task scope for review and reconnection continuity."`
}
type CommandOutput struct {
	Output             string  `json:"output"`
	SessionID          int64   `json:"session_id,omitempty"`
	Finished           bool    `json:"finished"`
	ExitCode           int     `json:"exit_code"`
	Error              string  `json:"error,omitempty"`
	Truncated          bool    `json:"truncated,omitempty"`
	OriginalTokenCount int     `json:"original_token_count"`
	WallTimeSeconds    float64 `json:"wall_time_seconds"`
}

func NewSessionManager() *SessionManager { return &SessionManager{sessions: map[int64]*Session{}} }
func randomID() (string, error) {
	b := make([]byte, 20)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}

// Keep the original numeric session_id schema but stop handing out trivially
// guessable process handles across clients sharing the same Bearer principal.
// 52 bits fit into the exact-integer range of JavaScript clients.
func randomNumericSessionID() (int64, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(raw[:]) & ((uint64(1) << 52) - 1)), nil
}

func (s *Session) isFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.finished.IsZero()
}

func (m *SessionManager) start(c Config, in CommandInput) (*Session, error) {
	return m.startForOwner(c, in, "")
}

func (m *SessionManager) startForOwner(c Config, in CommandInput, owner string) (*Session, error) {
	if in.Cmd == "" {
		return nil, errors.New("cmd is required")
	}
	if in.TTY {
		return nil, errors.New("tty=true is unsupported; command sessions use plain pipes")
	}
	directory := in.Workdir
	if directory == "" {
		directory = c.WorkRoot
	}
	path, err := resolveWorkPath(c.WorkRoot, directory)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		return nil, errors.New("workdir is not an accessible directory")
	}
	timeout := c.CommandTimeoutSec
	if in.TimeoutSeconds > 0 {
		if in.TimeoutSeconds > timeout {
			return nil, errors.New("requested timeout exceeds current maximum")
		}
		timeout = in.TimeoutSeconds
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, errors.New("shell sessions are shutting down")
	}
	for id, s := range m.sessions {
		s.mu.Lock()
		expired := !s.finished.IsZero() && time.Since(s.finished) > 30*time.Minute
		s.mu.Unlock()
		if expired {
			delete(m.sessions, id)
		}
	}
	live := 0
	for _, s := range m.sessions {
		if !s.isFinished() {
			live++
		}
	}
	if live >= 8 {
		m.mu.Unlock()
		return nil, errors.New("too many active shell sessions")
	}
	var id int64
	for attempts := 0; attempts < 8; attempts++ {
		id, err = randomNumericSessionID()
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if id >= 1<<40 && m.sessions[id] == nil {
			break
		}
		id = 0
	}
	if id == 0 {
		m.mu.Unlock()
		return nil, errors.New("could not allocate a unique shell session ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	shell := defaultExecShell()
	if in.Shell != "" {
		switch in.Shell {
		case "sh", "/bin/sh":
			shell = "/bin/sh"
		case "bash", "/bin/bash":
			if found, e := exec.LookPath("bash"); e == nil {
				shell = found
			} else {
				cancel()
				m.mu.Unlock()
				return nil, errors.New("bash is not installed")
			}
		default:
			cancel()
			m.mu.Unlock()
			return nil, errors.New("only sh or bash is allowed")
		}
	}
	cmd := executionCommand(ctx, shell, "-c", in.Cmd)
	cmd.Dir = path
	cmd.Env = executionEnvironment(c)
	limit := c.MaxOutputBytes
	if in.MaxOutputTokens > 0 && in.MaxOutputTokens <= limit/4 {
		limit = in.MaxOutputTokens * 4
	}
	writer := &boundedBuffer{limit: limit}
	cmd.Stdout = writer
	cmd.Stderr = writer
	pipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		m.mu.Unlock()
		return nil, err
	}
	// StdinPipe is an OS pipe. Refuse a non-pollable replacement rather than
	// falling back to an uninterruptible write or a detached writer goroutine.
	stdin, ok := pipe.(*os.File)
	if !ok {
		pipe.Close()
		if reader, ok := cmd.Stdin.(io.Closer); ok {
			reader.Close()
		}
		cancel()
		m.mu.Unlock()
		return nil, errors.New("shell stdin is not an OS pipe")
	}
	if err = stdin.SetWriteDeadline(time.Time{}); err != nil {
		stdin.Close()
		if reader, ok := cmd.Stdin.(io.Closer); ok {
			reader.Close()
		}
		cancel()
		m.mu.Unlock()
		return nil, fmt.Errorf("shell stdin does not support write deadlines: %w", err)
	}
	if err = cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		m.mu.Unlock()
		return nil, err
	}
	s := &Session{ID: id, owner: owner, started: time.Now(), command: cmd, stdin: stdin, inputGate: make(chan struct{}, 1), cancel: cancel, output: writer, done: make(chan struct{})}
	m.sessions[id] = s
	m.mu.Unlock()
	go func() {
		e := waitExecution(cmd)
		s.mu.Lock()
		if e != nil {
			s.errorMessage = e.Error()
			if x, ok := e.(*exec.ExitError); ok {
				s.exitCode = x.ExitCode()
			} else {
				s.exitCode = -1
			}
		}
		s.finished = time.Now()
		s.mu.Unlock()
		stdin.Close()
		cancel()
		close(s.done)
	}()
	return s, nil
}

func (m *SessionManager) get(id int64) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return nil, errors.New("unknown or expired session ID")
	}
	return s, nil
}

func (m *SessionManager) getForOwner(id int64, owner string) (*Session, error) {
	s, err := m.get(id)
	if err != nil {
		return nil, err
	}
	if s.owner != "" && s.owner != owner {
		// Do not reveal whether the handle exists to another client.
		return nil, errors.New("unknown or expired session ID")
	}
	return s, nil
}
func (s *Session) snapshot() CommandOutput {
	s.mu.Lock()
	out := CommandOutput{ExitCode: s.exitCode, Finished: !s.finished.IsZero(), Error: s.errorMessage, WallTimeSeconds: time.Since(s.started).Seconds()}
	if !out.Finished {
		out.SessionID = s.ID
	}
	s.mu.Unlock()
	out.Output, out.Truncated = s.output.Drain()
	out.OriginalTokenCount = (len(out.Output) + 3) / 4
	return out
}
func (s *Session) wait(ms int) CommandOutput {
	out, _ := s.waitContext(context.Background(), ms)
	return out
}

func (s *Session) waitContext(ctx context.Context, ms int) (CommandOutput, error) {
	if err := ctx.Err(); err != nil {
		return CommandOutput{}, err
	}
	if ms <= 0 {
		ms = 10000
	}
	if ms > 30000 {
		ms = 30000
	}
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return CommandOutput{}, ctx.Err()
	case <-s.done:
	case <-timer.C:
	}
	// A canceled poll must not drain output that a later request can resume.
	if err := ctx.Err(); err != nil {
		return CommandOutput{}, err
	}
	return s.snapshot(), nil
}

func (s *Session) write(chars string) error {
	_, err := s.writeContext(context.Background(), chars)
	return err
}

func (s *Session) writeContext(ctx context.Context, chars string) (n int, err error) {
	defer func() {
		if err != nil {
			// These are bytes accepted by the pipe, not a command-consumption
			// acknowledgement. A failed request must never invite whole replay.
			err = fmt.Errorf("stdin wrote %d/%d bytes; input may have been consumed; do not replay automatically: %w", n, len(chars), err)
		}
	}()
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	// Serialize both the write and its deadline cleanup. Waiting for a prior
	// request to release the pipe is itself request-cancelable.
	select {
	case s.inputGate <- struct{}{}:
		defer func() { <-s.inputGate }()
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.done:
		return 0, errors.New("session finished")
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	finished := !s.finished.IsZero()
	stdin := s.stdin
	s.mu.Unlock()
	if finished {
		return 0, errors.New("session finished")
	}
	deadline, _ := ctx.Deadline()
	if err = stdin.SetWriteDeadline(deadline); err != nil {
		return 0, err
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = stdin.SetWriteDeadline(time.Now())
		close(interrupted)
	})
	n, err = io.WriteString(stdin, chars)
	if !stop() {
		// stop does not join an already-started AfterFunc. Join it before
		// clearing the deadline or it could poison the next request's write.
		<-interrupted
	}
	clearErr := stdin.SetWriteDeadline(time.Time{})
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	} else if err != nil && errors.Is(err, os.ErrDeadlineExceeded) && !deadline.IsZero() && !time.Now().Before(deadline) {
		// The pipe timer can fire just before the context timer is scheduled.
		err = context.DeadlineExceeded
	} else if err == nil && clearErr != nil && !errors.Is(clearErr, os.ErrClosed) {
		err = clearErr
	}
	return n, err
}

func (s *Session) stop() {
	if s.isFinished() {
		return
	}
	s.cancel()
}
func (m *SessionManager) shutdown() error {
	m.mu.Lock()
	m.stopping = true
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.stop()
	}
	timer := time.NewTimer(executionShutdownTimeout)
	defer timer.Stop()
	for _, s := range sessions {
		select {
		case <-s.done:
		case <-timer.C:
			return errors.New("shell shutdown timed out waiting for processes")
		}
	}
	return nil
}
