package ralph

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeSpawn is a scripted Spawn: it returns the outputs in order, returning a
// per-call error when one is recorded, and reflects a cancelled context the way
// a real spawn capability would (so engine-level ctx failures surface).
type fakeSpawn struct {
	outputs []string
	errs    []error // parallel to outputs; nil entry means no error
	calls   int
}

func (f *fakeSpawn) spawn(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.calls >= len(f.outputs) {
		return "", errors.New("fakeSpawn: no more outputs")
	}
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return f.outputs[i], f.errs[i]
	}
	return f.outputs[i], nil
}

func mustEngine(t *testing.T, f *fakeSpawn) *Engine {
	t.Helper()
	eng, err := NewEngine(f.spawn)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

// TestRunDoneFirstRound: a DONE reply on the first round finishes immediately
// with the final deliverable and a single brief.
func TestRunDoneFirstRound(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{`{"status":"complete","summary":"完成报告","evidence":["verified"],"nextSteps":[],"blocker":""}`}})
	rep, err := eng.Run(context.Background(), "目标", 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Done {
		t.Fatal("rep.Done = false, want true")
	}
	if rep.Blocked {
		t.Fatal("rep.Blocked = true, want false")
	}
	if !strings.Contains(rep.Final, "完成报告") {
		t.Errorf("rep.Final = %q, want it to contain 完成报告", rep.Final)
	}
	if rep.Rounds != 1 {
		t.Errorf("rep.Rounds = %d, want 1", rep.Rounds)
	}
	if rep.MaxRounds != DefaultMaxRounds {
		t.Errorf("rep.MaxRounds = %d, want %d (default when max_rounds absent)", rep.MaxRounds, DefaultMaxRounds)
	}
	if len(rep.RoundBriefs) != 1 {
		t.Fatalf("len(rep.RoundBriefs) = %d, want 1", len(rep.RoundBriefs))
	}
	if rep.RoundBriefs[0] != rep.Final {
		t.Errorf("RoundBriefs[0] = %q, want the final %q", rep.RoundBriefs[0], rep.Final)
	}
}

// TestRunBlocked: a BLOCKED reply finishes immediately with the block reason.
func TestRunBlocked(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{`{"status":"blocked","summary":"无法继续","evidence":[],"nextSteps":[],"blocker":"缺凭证"}`}})
	rep, err := eng.Run(context.Background(), "目标", 3)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Blocked {
		t.Fatal("rep.Blocked = false, want true")
	}
	if rep.Done {
		t.Fatal("rep.Done = true, want false")
	}
	if rep.BlockReason != "缺凭证" {
		t.Errorf("rep.BlockReason = %q, want 缺凭证", rep.BlockReason)
	}
	if rep.Rounds != 1 {
		t.Errorf("rep.Rounds = %d, want 1", rep.Rounds)
	}
}

func TestRunStructuredReport(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{
		`{"status":"continue","summary":"implemented core","evidence":["core changed"],"nextSteps":["run tests"],"blocker":""}`,
		`{"status":"complete","summary":"all tests pass","evidence":["ready"],"nextSteps":[],"blocker":""}`,
	}})
	rep, err := eng.Run(context.Background(), "ship", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Done || rep.Final != "all tests pass" || len(rep.RoundReports) != 2 {
		t.Fatalf("structured report = %+v", rep)
	}
	if rep.RoundReports[0].Status != "continue" || len(rep.RoundReports[0].NextSteps) != 1 {
		t.Fatalf("first next steps = %+v", rep.RoundReports[0])
	}
}

func TestRunDSHStructuredReport(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{
		`{"status":"continue","summary":"implemented core","evidence":["go test passes"],"nextSteps":["run integration tests"],"blocker":""}`,
		`{"status":"complete","summary":"finished","evidence":["integration tests pass"],"nextSteps":[],"blocker":""}`,
	}})
	rep, err := eng.Run(context.Background(), "ship", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Done || len(rep.RoundReports) != 2 || len(rep.RoundReports[0].Evidence) != 1 || len(rep.RoundReports[0].NextSteps) != 1 {
		t.Fatalf("dsh structured report = %+v", rep)
	}
}

// TestRunProgressThenDone: a progress report carries the loop into round two,
// where the DONE reply ends it with both briefs recorded.
func TestRunProgressThenDone(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{
		`{"status":"continue","summary":"进展一","evidence":["work"],"nextSteps":["finish"],"blocker":""}`,
		`{"status":"complete","summary":"最终","evidence":["done"],"nextSteps":[],"blocker":""}`,
	}})
	rep, err := eng.Run(context.Background(), "目标", 3)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Done {
		t.Fatal("rep.Done = false, want true")
	}
	if rep.Rounds != 2 {
		t.Errorf("rep.Rounds = %d, want 2", rep.Rounds)
	}
	if len(rep.RoundBriefs) != 2 {
		t.Fatalf("len(rep.RoundBriefs) = %d, want 2", len(rep.RoundBriefs))
	}
	if rep.RoundBriefs[0] != "进展一" {
		t.Errorf("RoundBriefs[0] = %q, want the progress brief 进展一", rep.RoundBriefs[0])
	}
	if rep.Final != "最终" {
		t.Errorf("rep.Final = %q, want 最终", rep.Final)
	}
}

// TestRunRoundLimit: all-progress rounds exhaust the explicit cap without a
// DONE/BLOCKED outcome (a normal, non-error end).
func TestRunRoundLimit(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{
		`{"status":"continue","summary":"进展一","evidence":["one"],"nextSteps":["two"],"blocker":""}`,
		`{"status":"continue","summary":"进展二","evidence":["two"],"nextSteps":["three"],"blocker":""}`,
		`{"status":"continue","summary":"进展三","evidence":["three"],"nextSteps":["more"],"blocker":""}`,
	}})
	rep, err := eng.Run(context.Background(), "目标", 3)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Done || rep.Blocked {
		t.Fatalf("rep.Done=%v rep.Blocked=%v, want both false (round-limit)", rep.Done, rep.Blocked)
	}
	if rep.Rounds != 3 {
		t.Errorf("rep.Rounds = %d, want 3 (explicit cap)", rep.Rounds)
	}
	if len(rep.RoundBriefs) != 3 {
		t.Errorf("len(rep.RoundBriefs) = %d, want 3", len(rep.RoundBriefs))
	}
}

// TestRunMaxRoundsParam: an explicit max_rounds is honored as the loop cap.
func TestRunMaxRoundsParam(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{
		`{"status":"continue","summary":"a","evidence":["a"],"nextSteps":["b"],"blocker":""}`,
		`{"status":"continue","summary":"b","evidence":["b"],"nextSteps":["c"],"blocker":""}`,
		`{"status":"continue","summary":"c","evidence":["c"],"nextSteps":["d"],"blocker":""}`,
		`{"status":"continue","summary":"d","evidence":["d"],"nextSteps":["e"],"blocker":""}`,
		`{"status":"continue","summary":"e","evidence":["e"],"nextSteps":["more"],"blocker":""}`,
	}})
	rep, err := eng.Run(context.Background(), "目标", 5)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Rounds != 5 {
		t.Errorf("rep.Rounds = %d, want 5", rep.Rounds)
	}
	if rep.MaxRounds != 5 {
		t.Errorf("rep.MaxRounds = %d, want 5", rep.MaxRounds)
	}
}

// TestRunSpawnError: a spawn capability error (not a worker report) surfaces as
// an engine-level error.
func TestRunSpawnError(t *testing.T) {
	eng := mustEngine(t, &fakeSpawn{outputs: []string{"x"}, errs: []error{errors.New("spawn failed")}})
	_, err := eng.Run(context.Background(), "目标", 3)
	if err == nil {
		t.Fatal("Run: want an error for a failed spawn")
	}
	if !strings.Contains(err.Error(), "spawn failed") {
		t.Errorf("Run error = %v, want it to contain spawn failed", err)
	}
}

// TestRunContextCancel: a cancelled context surfaces as the ctx error (the
// engine-level failure path), not as a worker report.
func TestRunContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	eng := mustEngine(t, &fakeSpawn{outputs: []string{"anything"}})
	_, err := eng.Run(ctx, "目标", 3)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run error = %v, want context.Canceled", err)
	}
}

// TestBoundRunes: over-max text is cut with "…", at-max and empty text pass
// through, and the cut is rune-safe.
func TestBoundRunes(t *testing.T) {
	if got := boundRunes("abcdef", 3); got != "abc…" {
		t.Errorf("boundRunes(abcdef, 3) = %q, want abc…", got)
	}
	if got := boundRunes("abc", 3); got != "abc" {
		t.Errorf("boundRunes(abc, 3) = %q, want abc", got)
	}
	if got := boundRunes("", 3); got != "" {
		t.Errorf("boundRunes(\"\", 3) = %q, want empty", got)
	}
	if got := boundRunes("你好世界", 2); got != "你好…" {
		t.Errorf("boundRunes(你好世界, 2) = %q, want 你好…", got)
	}
}

// TestWorkerPromptContent: the worker prompt carries the objective, the round
// number, and the previous round's brief; the first round substitutes "（无）".
func TestWorkerPromptContent(t *testing.T) {
	prompt := buildWorkerPrompt("交付目标", 2, `{"status":"continue","summary":"上一轮进展","evidence":["checked"],"nextSteps":["继续"],"blocker":""}`)
	for _, want := range []string{"交付目标", "Ralph round: 2", "Previous structured handoff:", "上一轮进展"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("structured worker prompt lacks %q: %s", want, prompt)
		}
	}
	return
	p1 := buildWorkerPrompt("交付目标", 1, "")
	for _, want := range []string{"交付目标", "第 1 轮", "（无）"} {
		if !strings.Contains(p1, want) {
			t.Errorf("round-1 prompt lacks %q:\n%s", want, p1)
		}
	}
	p2 := buildWorkerPrompt("交付目标", 2, "上一轮进展")
	if !strings.Contains(p2, "第 2 轮") {
		t.Errorf("round-2 prompt lacks the round number:\n%s", p2)
	}
	if !strings.Contains(p2, "上一轮进展") {
		t.Errorf("round-2 prompt lacks the previous brief:\n%s", p2)
	}
	if strings.Contains(p2, "（无）") {
		t.Errorf("round-2 prompt must not contain （无） when a previous brief exists:\n%s", p2)
	}
}

// TestNewEngineNilSpawn: a nil spawn capability is rejected at construction.
func TestNewEngineNilSpawn(t *testing.T) {
	if _, err := NewEngine(nil); err == nil {
		t.Fatal("NewEngine(nil) must return an error")
	}
}
