package team

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jabing/shutu-agent/internal/agent"
)

func TestRosterProvisionsDirectAgentAndAuthorizesMembers(t *testing.T) {
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	lead, err := registry.Create(agent.Options{ID: "lead", Runner: func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := lead.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	roster, err := NewRoster("team-1", "lead", registry)
	if err != nil {
		t.Fatal(err)
	}
	member, err := roster.Spawn(context.Background(), "lead", "worker", "inspect", "spawn", "fresh", func(context.Context, *agent.Agent, agent.TurnInput) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if member.Phase != MemberActive || member.Role != "teammate" || member.Status != agent.StatusIdle {
		t.Fatalf("member = %+v", member)
	}
	if err := roster.Authorize(agent.ID(member.ID), agent.ID(member.ID)); err != nil {
		t.Fatalf("member should address itself: %v", err)
	}
	if err := roster.Authorize("intruder", agent.ID(member.ID)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("intruder authorization = %v", err)
	}
	if _, err := roster.Spawn(context.Background(), agent.ID(member.ID), "other", "", "", "fresh", func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }); !errors.Is(err, ErrNotLead) {
		t.Fatalf("non-lead spawn = %v", err)
	}
}

func TestRosterUsesDurableMemberReservationBeforeRegistryPublication(t *testing.T) {
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	lead, err := registry.Create(agent.Options{ID: "lead", Runner: func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := lead.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	roster, err := NewRoster("team-reserved", "lead", registry)
	if err != nil {
		t.Fatal(err)
	}
	var namespace, reservedID string
	roster.SetIDReservation(func(gotNamespace, gotID string) (bool, error) {
		namespace, reservedID = gotNamespace, gotID
		return true, nil
	})
	if got, err := roster.ReserveMemberID("worker"); err != nil || got != "team-reserved:worker" {
		t.Fatalf("ReserveMemberID = %q, %v", got, err)
	}
	if namespace != "team-member:team-reserved" || reservedID != "team-reserved:worker" {
		t.Fatalf("reservation = namespace %q id %q", namespace, reservedID)
	}
	member, err := roster.Spawn(context.Background(), "lead", "worker", "", "", "fresh", func(context.Context, *agent.Agent, agent.TurnInput) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if member.ID != reservedID || member.Phase != MemberActive {
		t.Fatalf("member = %+v, reserved id = %q", member, reservedID)
	}
}

func TestRosterFailedProvisioningKeepsNameAndRestores(t *testing.T) {
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	lead, err := registry.Create(agent.Options{ID: "lead", Runner: func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := lead.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	roster, err := NewRoster("team-2", "lead", registry)
	if err != nil {
		t.Fatal(err)
	}
	// A duplicate registry identity forces the provisioning edge to fail.
	if _, err := registry.Create(agent.Options{ID: "team-2:worker", Runner: func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if _, err := roster.Spawn(context.Background(), "lead", "worker", "", "", "fresh", func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }); err == nil {
		t.Fatal("duplicate identity must fail provisioning")
	}
	if _, err := roster.Spawn(context.Background(), "lead", "worker", "", "", "fresh", func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }); !errors.Is(err, ErrMemberExists) {
		t.Fatalf("failed member name was reusable: %v", err)
	}
	rows := roster.Snapshot()
	if len(rows) != 2 || rows[1].Phase != MemberFailed {
		t.Fatalf("roster snapshot = %+v", rows)
	}
	restored, err := NewRoster("team-2", "lead", registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(rows); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Member("team-2:worker"); err != nil {
		t.Fatalf("restored failed member missing: %v", err)
	}
}

func TestBoardSnapshotCarriesRosterAndRejectsUnknownActor(t *testing.T) {
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	lead, err := registry.Create(agent.Options{ID: "lead", Runner: func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := lead.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	roster, err := NewRoster("team-3", "lead", registry)
	if err != nil {
		t.Fatal(err)
	}
	board, err := New("team-3", nil)
	if err != nil {
		t.Fatal(err)
	}
	board.AttachRoster(roster)
	if _, err := board.CreateTask("visible", "", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(board.Snapshot().Members) != 1 {
		t.Fatalf("snapshot members = %+v", board.Snapshot().Members)
	}
}

func TestRosterRebindRestoredActiveMember(t *testing.T) {
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	lead, err := registry.Create(agent.Options{ID: "lead", Runner: func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := lead.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	roster, err := NewRoster("team-rebind", "lead", registry)
	if err != nil {
		t.Fatal(err)
	}
	member, err := roster.Spawn(context.Background(), "lead", "worker", "", "", "fresh", func(context.Context, *agent.Agent, agent.TurnInput) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	rows := roster.Snapshot()
	if err := registry.Close(agent.ID(member.ID)); err != nil {
		t.Fatal(err)
	}
	restored, err := NewRoster("team-rebind", "lead", registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(rows); err != nil {
		t.Fatal(err)
	}
	view, err := restored.Rebind(context.Background(), agent.ID(member.ID), func(context.Context, *agent.Agent, agent.TurnInput) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != agent.StatusIdle {
		t.Fatalf("rebound member status = %s, want idle", view.Status)
	}
	if _, err := restored.Handle(agent.ID(member.ID)); err != nil {
		t.Fatalf("rebound handle unavailable: %v", err)
	}
}

func TestRosterRebindProvisioningMemberPromotesOnlyAfterStart(t *testing.T) {
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	lead, err := registry.Create(agent.Options{ID: "lead", Runner: func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := lead.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	roster, err := NewRoster("team-provisioning-recovery", "lead", registry)
	if err != nil {
		t.Fatal(err)
	}
	row := MemberSnapshot{ID: "team-provisioning-recovery:worker", Name: "worker", Provider: "spawn", Context: "fresh", Phase: MemberProvisioning}
	if err := roster.ApplyMemberEvent(row); err != nil {
		t.Fatal(err)
	}
	view, err := roster.RebindProvisioning(context.Background(), agent.ID(row.ID), func(context.Context, *agent.Agent, agent.TurnInput) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if view.Phase != MemberActive || view.Status != agent.StatusIdle {
		t.Fatalf("recovered member = %+v", view)
	}
	if got, err := roster.Member(agent.ID(row.ID)); err != nil || got.Phase != MemberActive {
		t.Fatalf("roster member = %+v err=%v", got, err)
	}
}

func TestRosterValidatesNamesLimitsAndTargets(t *testing.T) {
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	lead, err := registry.Create(agent.Options{ID: "lead", Runner: func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := lead.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	roster, err := NewRosterWithLimit("team-limits", "lead", registry, 1)
	if err != nil {
		t.Fatal(err)
	}
	runner := func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }
	if _, err := roster.Spawn(context.Background(), "lead", "Not-Kebab", "", "", "fresh", runner); err == nil {
		t.Fatal("invalid member name was accepted")
	}
	member, err := roster.Spawn(context.Background(), "lead", "worker-one", "", "", "fresh", runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roster.Spawn(context.Background(), "lead", "worker-two", "", "", "fresh", runner); err == nil {
		t.Fatal("member limit was not enforced")
	}
	if _, err := roster.ResolveTarget("worker-one"); err != nil {
		t.Fatalf("name target = %v", err)
	}
	if _, err := roster.ResolveTarget("missing"); err == nil {
		t.Fatal("unknown target was accepted")
	}
	if member.ID == "" {
		t.Fatal("member id is empty")
	}
}

func TestBoardFoldsMemberLifecycleEventsWithoutFabricatingHandle(t *testing.T) {
	registry := agent.NewRegistry()
	defer registry.CloseAll()
	lead, err := registry.Create(agent.Options{ID: "lead", Runner: func(context.Context, *agent.Agent, agent.TurnInput) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := lead.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	roster, err := NewRoster("team-members", "lead", registry)
	if err != nil {
		t.Fatal(err)
	}
	board, err := New("team-members", nil)
	if err != nil {
		t.Fatal(err)
	}
	board.AttachRoster(roster)
	member := MemberSnapshot{ID: "team-members:worker", Name: "worker", Provider: "agent-registry", Context: "fresh", Phase: MemberProvisioning}
	for _, row := range []MemberSnapshot{member, func() MemberSnapshot { active := member; active.Phase = MemberActive; return active }()} {
		data, err := json.Marshal(MemberEvent{Version: 1, TeamID: board.TeamID(), Member: row})
		if err != nil {
			t.Fatal(err)
		}
		if err := board.ApplyEvent("team/member", data); err != nil {
			t.Fatal(err)
		}
	}
	view, err := roster.Member(agent.ID(member.ID))
	if err != nil || view.Phase != MemberActive {
		t.Fatalf("folded member = %+v err=%v", view, err)
	}
	if _, err := roster.Handle(agent.ID(member.ID)); !errors.Is(err, ErrMemberNotFound) {
		t.Fatalf("folded member fabricated live handle: %v", err)
	}
}
