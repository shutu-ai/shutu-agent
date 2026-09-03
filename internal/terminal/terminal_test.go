package terminal

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shutu-ai/shutu-agent/internal/pathsecure"
)

// testOpts returns short idle/timeout windows so the real-subprocess tests are
// fast and stable on a local Windows host.
func testOpts() SessionOpts {
	return SessionOpts{IdleMS: 100, TimeoutMS: 2500}
}

// waitKind polls s.Status() until its Kind equals kind, up to d.
func waitKind(s *Session, kind string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s.Status().Kind == kind {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return s.Status().Kind == kind
}

func TestNewSession(t *testing.T) {
	s, err := NewSession(SessionOpts{})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer s.Close()

	if s.ID() == "" {
		t.Error("ID() is empty")
	}
	if d := time.Since(s.StartedAt()); d < 0 || d > 10*time.Second {
		t.Errorf("StartedAt() not recent: %v ago", d)
	}
	if st := s.Status(); st.Kind != "running" {
		t.Errorf("Status().Kind = %q, want %q", st.Kind, "running")
	}
}

// waitViewport polls the scrollback until text appears or d elapses. The
// idle-based Write may return before a slow shell (cold cmd.exe under heavy
// parallel test load) echoes anything — D-M9-4's readiness contract is
// silence, not output — so viewport assertions poll before failing
// (TestReadConsume's pattern).
func waitViewport(s *Session, text string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	want := strings.ToLower(text)
	for time.Now().Before(deadline) {
		all, _ := s.Read(0, 999)
		if strings.Contains(strings.ToLower(all), want) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	all, _ := s.Read(0, 999)
	return strings.Contains(strings.ToLower(all), want)
}

func TestWriteEcho(t *testing.T) {
	s, err := NewSession(testOpts())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer s.Close()

	res, err := s.Write("echo hello", true)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if res.Wait != WaitStdinRead {
		t.Errorf("Write().Wait = %q, want %q", res.Wait, WaitStdinRead)
	}
	if !strings.Contains(res.Viewport, "hello") && !waitViewport(s, "hello", 5*time.Second) {
		t.Errorf("Viewport = %q, want contains %q", res.Viewport, "hello")
	}
}

func TestCwdPersists(t *testing.T) {
	s, err := NewSession(testOpts())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer s.Close()

	// EvalSymlinks resolves the 8.3 short form (e.g. SWX153~1) to the long path;
	// `cd /d` is required because the session starts on D: while t.TempDir() is
	// on C:, and a bare cmd `cd` does not change drives.
	dir, err := pathsecure.ResolveExisting(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temp) error = %v", err)
	}
	var changeCommand, printCommand, want string
	if runtime.GOOS == "windows" {
		changeCommand = `cd /d "` + dir + `"`
		printCommand = "cd"
		want = strings.ReplaceAll(dir, "/", "\\")
	} else {
		changeCommand = "cd '" + dir + "'"
		printCommand = "pwd"
		want = dir
	}
	if _, err := s.Write(changeCommand, true); err != nil {
		t.Fatalf("Write(cd dir) error = %v", err)
	}
	res, err := s.Write(printCommand, true)
	if err != nil {
		t.Fatalf("Write(cd) error = %v", err)
	}

	if !strings.Contains(strings.ToLower(res.Viewport), strings.ToLower(want)) &&
		!waitViewport(s, want, 5*time.Second) {
		t.Errorf("Viewport = %q, want contains %q", res.Viewport, want)
	}
}

func TestReadConsume(t *testing.T) {
	s, err := NewSession(testOpts())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer s.Close()

	// Write drains the buffer into its own Viewport, so Read/Consume are tested
	// against output that arrives asynchronously after Write returns: the ping
	// is silent (>nul) and "pong" is echoed only ~1s later.
	command := "sleep 2; echo pong"
	if runtime.GOOS == "windows" {
		command = "ping -n 2 127.0.0.1 >nul & echo pong"
	}
	if _, err := s.Write(command, true); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	all := ""
	for {
		all, _ = s.Read(0, 999)
		if strings.Contains(all, "pong") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Read() never saw pong; last = %q", all)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(all, "pong") {
		t.Errorf("Read() = %q, want contains %q", all, "pong")
	}

	cons, _ := s.Consume()
	if !strings.Contains(cons, "pong") {
		t.Errorf("Consume() = %q, want contains %q", cons, "pong")
	}
	// Drain trailing output (prompt etc.) until the buffer is quiet again.
	drainDeadline := time.Now().Add(2 * time.Second)
	for {
		again, _ := s.Consume()
		if again == "" {
			break
		}
		if time.Now().After(drainDeadline) {
			t.Errorf("Consume() keeps returning %q, want empty", again)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWriteTimeout(t *testing.T) {
	// With IdleMS=100 any silent gap longer than 100ms resolves as WaitStdinRead,
	// so a genuine timeout needs output with gaps shorter than idleMS. ping emits
	// a reply every ~1s; IdleMS=1500 keeps it "active" until TimeoutMS=2500 fires.
	s, err := NewSession(SessionOpts{IdleMS: 1500, TimeoutMS: 2500})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer s.Close()

	command := "while true; do echo tick; sleep 0.1; done"
	if runtime.GOOS == "windows" {
		command = "ping -n 10 127.0.0.1"
	}
	res, err := s.Write(command, true)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if res.Wait != WaitTimeout {
		t.Errorf("Write().Wait = %q, want %q", res.Wait, WaitTimeout)
	}
}

func TestWriteContextCancelsForegroundCommand(t *testing.T) {
	// Keep the shell's idle window longer than the cancellation delay; idle is
	// only a heuristic that foreground work finished, not a kill signal.
	s, err := NewSession(SessionOpts{IdleMS: 1000, TimeoutMS: 5000})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	command := "sleep 10"
	if runtime.GOOS == "windows" {
		command = "ping -n 10 127.0.0.1"
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var res WriteResult
	var writeErr error
	go func() {
		defer close(done)
		res, writeErr = s.WriteContext(ctx, command, true)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WriteContext ignored caller cancellation")
	}
	if !errors.Is(writeErr, context.Canceled) {
		t.Fatalf("WriteContext error = %v, result=%+v, want context.Canceled", writeErr, res)
	}
	if res.Wait != WaitCancelled {
		t.Fatalf("WriteContext wait = %q, want %q", res.Wait, WaitCancelled)
	}
	if !waitKind(s, "exited", 3*time.Second) {
		t.Fatalf("cancelled terminal still %v; want reset", s.Status())
	}
	if _, err := s.Write("echo after-cancel", true); err == nil {
		t.Fatal("reset terminal accepted input")
	}
}

func TestSessionExit(t *testing.T) {
	s, err := NewSession(testOpts())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer s.Close()

	res, err := s.Write("exit", true)
	if err != nil {
		t.Fatalf("Write(exit) error = %v", err)
	}
	if !waitKind(s, "exited", 3*time.Second) {
		t.Errorf("after exit: Status().Kind = %q, want %q (Write wait=%q)", s.Status().Kind, "exited", res.Wait)
	}
	if _, err := s.Write("echo after", true); err == nil {
		t.Error("Write after exit: want error, got nil")
	} else if !strings.Contains(err.Error(), "session exited") {
		t.Errorf("Write after exit error = %q, want contains %q", err.Error(), "session exited")
	}
}

func TestSignalStop(t *testing.T) {
	s, err := NewSession(testOpts())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer s.Close()

	if err := s.Signal("stop"); err != nil {
		t.Fatalf("Signal(stop) error = %v", err)
	}
	if !waitKind(s, "exited", 3*time.Second) {
		t.Errorf("after Signal(stop): Status().Kind = %q, want %q", s.Status().Kind, "exited")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close() after stop error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close() error = %v", err)
	}
	if err := s.Signal("bogus"); err == nil {
		t.Error("Signal(bogus) want error, got nil")
	}
}

func TestScrubbedEnv(t *testing.T) {
	sensitive := []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "API"}
	seen := 0
	for _, kv := range scrubbedEnv() {
		name := strings.ToUpper(strings.SplitN(kv, "=", 2)[0])
		for _, bad := range sensitive {
			if strings.Contains(name, bad) {
				t.Errorf("scrubbedEnv() leaked sensitive var %q", name)
			}
		}
		seen++
	}
	if seen == 0 {
		t.Error("scrubbedEnv() returned no environment variables")
	}
}
