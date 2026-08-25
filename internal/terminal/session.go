package terminal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrNoActive reports that no terminal session exists for the current owner.
// The /term REPL's accessor surfaces it to the user ("start one with /term
// start"); the model-facing pwsh tool is a fresh process per call and never
// depends on a session.
var ErrNoActive = errors.New("no active terminal session")

// WaitReason 描述一次 Write 在哪个条件下就绪返回。
type WaitReason string

const (
	// WaitStdinRead 表示 shell 已静默（距最后一次输出超过 idleMS），
	// 可以安全地把下一批输入写入 stdin。
	WaitStdinRead WaitReason = "stdin_read"
	// WaitTimeout 表示等待超过 timeoutMS，未等到静默也未退出。
	WaitTimeout WaitReason = "timeout"
	// WaitSessionExit 表示 shell 会话已退出。
	WaitSessionExit WaitReason = "session_exit"
)

// SessionStatus 描述会话当前状态。
type SessionStatus struct {
	Kind     string // "running" | "exited"
	ExitCode int    // Kind == "exited" 时有效
}

// WriteResult 是一次 Write 的返回：累积视口输出 + 就绪原因 + 状态。
type WriteResult struct {
	Viewport  string
	Wait      WaitReason
	Truncated bool
	Status    SessionStatus
}

// SessionOpts 配置一个持久 shell 会话。
type SessionOpts struct {
	Shell              string
	Args               []string
	Workdir            string
	Env                []string
	IdleMS             int
	TimeoutMS          int
	ScrollbackMaxBytes int
	ScrollbackLines    int
}

// Session 是一个持久 shell 会话：子进程 stdin 经 StdinPipe 直连宿主写入，
// stdout/stderr 汇入一条输出管道，泵 goroutine 把输出追加进 BoundedTextBuffer，
// wait goroutine 在子进程退出时收尾。
type Session struct {
	id        string
	startedAt time.Time
	buf       *BoundedTextBuffer
	idleMS    int
	timeoutMS int
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	cancel    context.CancelFunc
	waitDone  chan struct{}
	exitCode  int

	mu         sync.Mutex // 保护 lastAppend / exited
	lastAppend time.Time
	exited     bool

	closeOnce sync.Once
}

// NewSession 创建并启动一个持久 shell 会话。Shell 为空时由平台
// shellCommand 选用默认（Windows: cmd.exe；其他: /bin/sh）。
func NewSession(opts SessionOpts) (*Session, error) {
	idleMS := opts.IdleMS
	if idleMS <= 0 {
		idleMS = 500
	}
	timeoutMS := opts.TimeoutMS
	if timeoutMS <= 0 {
		timeoutMS = 30000
	}
	maxBytes := opts.ScrollbackMaxBytes
	if maxBytes <= 0 {
		maxBytes = 65536
	}
	maxLines := opts.ScrollbackLines
	if maxLines <= 0 {
		maxLines = 2000
	}

	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("session: generate id: %w", err)
	}
	id := hex.EncodeToString(raw)

	buf := NewBoundedTextBuffer(maxBytes, maxLines)

	cmd := shellCommand(opts)
	cmd.Dir = opts.Workdir
	if len(opts.Env) > 0 {
		cmd.Env = opts.Env
	} else {
		cmd.Env = scrubbedEnv()
	}

	// 输入：宿主 -> 子进程 stdin。StdinPipe 由 exec 管理，子进程退出后自动
	// 关闭，不阻塞 Wait。
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("session: stdin pipe: %w", err)
	}
	// 输出：子进程 stdout/stderr 汇入同一输出管道，泵从读端 opr 读。
	opr, opw := io.Pipe()
	cmd.Stdout = opw
	cmd.Stderr = opw

	ctx, cancel := context.WithCancel(context.Background())

	s := &Session{
		id:        id,
		startedAt: time.Now(),
		buf:       buf,
		idleMS:    idleMS,
		timeoutMS: timeoutMS,
		cmd:       cmd,
		stdin:     stdin,
		cancel:    cancel,
		waitDone:  make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		opr.Close()
		opw.Close()
		return nil, fmt.Errorf("session: start shell: %w", err)
	}

	// 泵 goroutine：把子进程输出读进缓冲，并刷新 lastAppend。
	go func() {
		_, _ = io.Copy(bufWriter{s: s}, opr)
	}()

	// wait goroutine：等待子进程退出，记录退出码并收尾。
	go func() {
		waitErr := cmd.Wait()
		s.mu.Lock()
		s.exited = true
		if waitErr == nil {
			s.exitCode = 0
		} else if ee, ok := waitErr.(*exec.ExitError); ok {
			s.exitCode = ee.ExitCode()
		} else {
			s.exitCode = -1
		}
		s.mu.Unlock()
		close(s.waitDone)
		s.cancel()
	}()

	// ctx 取消时关闭输出管道，结束泵。
	go func() {
		<-ctx.Done()
		opr.Close()
		opw.Close()
	}()

	return s, nil
}

// ID 返回会话 id（hex 编码的 8 随机字节）。
func (s *Session) ID() string { return s.id }

// PID returns the child process id when the shell has started. It is exposed
// for dsh's terminal_signal result; on platforms without a process id it is 0.
func (s *Session) PID() int {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// StartedAt 返回会话创建时间。
func (s *Session) StartedAt() time.Time { return s.startedAt }

// Status 返回会话当前状态。
func (s *Session) Status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return SessionStatus{Kind: "exited", ExitCode: s.exitCode}
	}
	return SessionStatus{Kind: "running"}
}

// Write 向会话 stdin 写入 text（submit 为 true 时追加 CRLF），并轮询等待就绪：
// 静默（WaitStdinRead）、超时（WaitTimeout）或退出（WaitSessionExit）。
func (s *Session) Write(text string, submit bool) (WriteResult, error) {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return WriteResult{}, fmt.Errorf("session exited")
	}
	s.mu.Unlock()

	// 先清空缓冲，保证 Viewport 只包含本次写之后的新输出。
	s.buf.Consume()

	// 重置静默时钟，使就绪判定从本次写入起算。
	s.mu.Lock()
	s.lastAppend = time.Now()
	s.mu.Unlock()

	payload := text
	if submit {
		payload += "\r\n"
	}
	if _, err := s.stdin.Write([]byte(payload)); err != nil {
		return WriteResult{}, fmt.Errorf("session: write stdin: %w", err)
	}

	start := time.Now()
	var sb strings.Builder
	truncated := false
	for {
		part, trunc := s.buf.Consume()
		truncated = truncated || trunc
		sb.WriteString(part)

		if time.Since(start) >= time.Duration(s.timeoutMS)*time.Millisecond {
			return WriteResult{
				Viewport:  sb.String(),
				Wait:      WaitTimeout,
				Truncated: truncated,
				Status:    s.Status(),
			}, nil
		}

		s.mu.Lock()
		exited := s.exited
		last := s.lastAppend
		code := s.exitCode
		s.mu.Unlock()

		if exited {
			part, trunc := s.buf.Consume()
			truncated = truncated || trunc
			sb.WriteString(part)
			return WriteResult{
				Viewport:  sb.String(),
				Wait:      WaitSessionExit,
				Truncated: truncated,
				Status:    SessionStatus{Kind: "exited", ExitCode: code},
			}, nil
		}

		if time.Since(last) >= time.Duration(s.idleMS)*time.Millisecond {
			part, trunc := s.buf.Consume()
			truncated = truncated || trunc
			sb.WriteString(part)
			return WriteResult{
				Viewport:  sb.String(),
				Wait:      WaitStdinRead,
				Truncated: truncated,
				Status:    s.Status(),
			}, nil
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// Read 从滚动缓冲读取一个窗口：从尾部倒数 offset 行起，取 count 行。
// 返回的 bool 为 true 表示返回窗口之前仍有更早内容，或缓冲发生过截断。
func (s *Session) Read(offset, count int) (string, bool) {
	text, _, _, _, truncated := s.ReadWindow(offset, count)
	return text, truncated
}

// ReadWindow is the dsh terminal_read projection: it returns the selected
// newest-relative page together with retained line coordinates.
func (s *Session) ReadWindow(offset, count int) (text string, totalLines, lineBegin, lineEnd int, truncated bool) {
	if offset < 0 {
		offset = 0
	}
	if count < 0 {
		count = 0
	}
	text, snapshotTruncated := s.buf.Snapshot()
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if offset >= total {
		return "", total, offset, offset, true
	}
	start := total - offset - count
	if start < 0 {
		start = 0
	}
	end := start + count
	if end > total {
		end = total
	}
	return strings.Join(lines[start:end], "\n"), total, start, end, snapshotTruncated || start > 0
}

// Consume 返回自上次 Consume 以来累积的输出（委托给缓冲）。
func (s *Session) Consume() (string, bool) {
	return s.buf.Consume()
}

// Signal 向会话发送控制信号。
func (s *Session) Signal(kind string) error {
	switch kind {
	case "stop":
		return s.Close()
	case "interrupt":
		interruptInput(s)
		return nil
	default:
		return fmt.Errorf("session: unknown signal %q", kind)
	}
}

// Close 终止会话：杀进程树、等待退出（最多 2s）、取消上下文关闭输出管道。
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		killProcessTree(s.cmd)
		select {
		case <-s.waitDone:
		case <-time.After(2 * time.Second):
		}
		s.cancel()
	})
	return nil
}

// bufWriter 是泵的适配器：把从输出管道读到的块 Append 进缓冲并刷新 lastAppend。
type bufWriter struct{ s *Session }

func (bw bufWriter) Write(p []byte) (int, error) {
	bw.s.buf.Append(string(p))
	bw.s.mu.Lock()
	bw.s.lastAppend = time.Now()
	bw.s.mu.Unlock()
	return len(p), nil
}

// scrubbedEnv 返回环境变量，丢弃变量名（大写比较）含敏感子串者。
func scrubbedEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		if strings.Contains(name, "KEY") ||
			strings.Contains(name, "SECRET") ||
			strings.Contains(name, "TOKEN") ||
			strings.Contains(name, "PASSWORD") ||
			strings.Contains(name, "API") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
