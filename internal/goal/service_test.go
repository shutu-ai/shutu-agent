package goal

import (
	"context"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/plan"
	"github.com/jabing/shutu-agent/internal/session"
)

func TestDriverContinuesSameSessionUntilGoalDone(t *testing.T) {
	e := plan.NewEngine(nil)
	g, err := e.CreateGoal(context.Background(), "ship", "Ship the feature")
	if err != nil {
		t.Fatal(err)
	}
	log := session.New()
	seen := []string{}
	d := &Driver{Plans: e, Log: log, MaxRounds: 3, Runner: func(ctx context.Context, prompt string) error {
		seen = append(seen, prompt)
		return e.SetStatus(ctx, string(plan.ScopeGoal), g.ID, plan.StatusDone)
	}}
	res, err := d.Run(context.Background(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "completed" || res.Rounds != 1 {
		t.Fatalf("result=%+v", res)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], `"Ship the feature"`) || !strings.Contains(seen[0], "Round: 1/3") {
		t.Fatalf("prompt=%q", seen)
	}
	if len(log.Events()) != 2 || log.Events()[0].Type != session.EventGoalRoundStart || log.Events()[1].Type != session.EventGoalRoundEnd {
		t.Fatalf("goal events=%+v", log.Events())
	}
}

func TestDriverRoundLimitBlocksAndObserverIsFailOpen(t *testing.T) {
	e := plan.NewEngine(nil)
	g, err := e.CreateGoal(context.Background(), "wait", "Keep waiting")
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{Plans: e, MaxRounds: 2, Runner: func(context.Context, string) error { return nil }, Observe: func(context.Context, plan.Goal) (string, error) { return "", context.DeadlineExceeded }}
	res, err := d.Run(context.Background(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "round-limit" || res.Rounds != 2 || res.Status != plan.StatusBlocked {
		t.Fatalf("result=%+v", res)
	}
}

func TestDriverRejectsUnknownGoal(t *testing.T) {
	d := &Driver{Plans: plan.NewEngine(nil), Runner: func(context.Context, string) error { return nil }}
	if _, err := d.Run(context.Background(), "missing"); err == nil {
		t.Fatal("missing goal must fail")
	}
}
