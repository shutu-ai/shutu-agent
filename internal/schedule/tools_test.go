package schedule

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// eventRecord captures one (type, payload) pair forwarded through the onEvent
// sink of NewScheduleTools.
type eventRecord struct {
	typ string
	raw string
}

// collectEvents builds an onEvent sink that records every forwarded payload.
func collectEvents(recs *[]eventRecord) func(string, any) {
	return func(typ string, data any) {
		raw, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		*recs = append(*recs, eventRecord{typ: typ, raw: string(raw)})
	}
}

// countEventType returns how many recorded events carry typ.
func countEventType(recs []eventRecord, typ string) int {
	n := 0
	for _, r := range recs {
		if r.typ == typ {
			n++
		}
	}
	return n
}

// --- D7 schema shape ----------------------------------------------------------

// TestScheduleToolsSchemaShape verifies the D7 argument schemas shipped with
// the tools: schedule_create requires kind (restricted to the interval|cron
// enum) and spec and rejects unknown properties; schedule_list takes no
// arguments; schedule_delete requires a non-empty id. The registry compiles and
// enforces these (cmd/sta), so the shape is asserted here.
func TestScheduleToolsSchemaShape(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	tools := NewScheduleTools(e, nil)

	create := tools.Create().Schema()
	if create["type"] != "object" || create["additionalProperties"] != false {
		t.Fatalf("schedule_create schema = %v, want object + additionalProperties:false", create)
	}
	props, _ := create["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("schedule_create properties missing: %v", create)
	}
	kind, _ := props["kind"].(map[string]any)
	if kind == nil {
		t.Fatalf("schedule_create kind property missing: %v", create)
	}
	if kind["type"] != "string" {
		t.Fatalf("kind type = %v, want string", kind["type"])
	}
	enum, ok := kind["enum"].([]string)
	if !ok || !reflect.DeepEqual(enum, []string{string(KindInterval), string(KindCron)}) {
		t.Fatalf("kind enum = %v, want [interval cron]", kind["enum"])
	}
	required, _ := create["required"].([]string)
	if !reflect.DeepEqual(required, []string{"kind", "spec"}) {
		t.Fatalf("schedule_create required = %v, want [kind spec]", required)
	}

	list := tools.List().Schema()
	if list["type"] != "object" || list["additionalProperties"] != false {
		t.Fatalf("schedule_list schema = %v, want object + additionalProperties:false", list)
	}
	if p, _ := list["properties"].(map[string]any); len(p) != 0 {
		t.Fatalf("schedule_list properties = %v, want empty", p)
	}

	del := tools.Delete().Schema()
	if del["type"] != "object" || del["additionalProperties"] != false {
		t.Fatalf("schedule_delete schema = %v, want object + additionalProperties:false", del)
	}
	if req, _ := del["required"].([]string); !reflect.DeepEqual(req, []string{"id"}) {
		t.Fatalf("schedule_delete required = %v, want [id]", req)
	}
}

// --- create -------------------------------------------------------------------

// TestScheduleCreateToolStoresAndEmits verifies schedule_create stores an
// interval and a cron trigger through the Engine and emits exactly one
// schedule/create event carrying the provider-issued id, kind and spec (D3,
// serial tool path).
func TestScheduleCreateToolStoresAndEmits(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	var recs []eventRecord
	tools := NewScheduleTools(e, collectEvents(&recs))

	out, err := tools.Create().Execute(context.Background(), json.RawMessage(`{"kind":"interval","spec":"30m","payload":"ping every 30m"}`))
	if err != nil {
		t.Fatalf("create interval: %v", err)
	}
	if !strings.Contains(out, "created schedule ") || !strings.Contains(out, "sched-1") {
		t.Fatalf("create output = %q, want created schedule sched-1", out)
	}
	out2, err := tools.Create().Execute(context.Background(), json.RawMessage(`{"kind":"cron","spec":"0 9 * * *","payload":"morning report"}`))
	if err != nil {
		t.Fatalf("create cron: %v", err)
	}
	if !strings.Contains(out2, "sched-2") {
		t.Fatalf("create output = %q, want sched-2", out2)
	}
	if n := countEventType(recs, "schedule/create"); n != 2 {
		t.Fatalf("schedule/create count = %d, want 2", n)
	}
	var first struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Spec string `json:"spec"`
	}
	if err := json.Unmarshal([]byte(recs[0].raw), &first); err != nil {
		t.Fatalf("unmarshal schedule/create: %v", err)
	}
	if first.ID != "sched-1" || first.Kind != string(KindInterval) || first.Spec != "30m" {
		t.Fatalf("schedule/create payload = %+v", first)
	}
}

// TestScheduleCreateToolRejectsBadKind verifies the repeated kind guard: a
// direct call with a kind outside {interval, cron} is rejected before any
// record is stored (D7 is enforced by the registry; this is the tool-layer
// defense).
func TestScheduleCreateToolRejectsBadKind(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	tools := NewScheduleTools(e, nil)
	for _, args := range []string{
		`{"kind":"weekly","spec":"30m"}`,
		`{"kind":"","spec":"30m"}`,
	} {
		if _, err := tools.Create().Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("schedule_create with %s must be rejected", args)
		}
	}
	if all, _ := e.List(context.Background()); len(all) != 0 {
		t.Fatalf("rejected kinds still stored %d schedules", len(all))
	}
}

// TestScheduleCreateToolRejectsInvalidSpec verifies invalid specs are rejected
// at Add time and nothing is stored: a zero interval and a malformed cron
// expression.
func TestScheduleCreateToolRejectsInvalidSpec(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	tools := NewScheduleTools(e, nil)
	for _, args := range []string{
		`{"kind":"interval","spec":"0s"}`,
		`{"kind":"interval","spec":"bogus"}`,
		`{"kind":"cron","spec":"not a cron"}`,
	} {
		if _, err := tools.Create().Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("schedule_create with %s must be rejected", args)
		}
	}
	if all, _ := e.List(context.Background()); len(all) != 0 {
		t.Fatalf("invalid specs still stored %d schedules", len(all))
	}
}

// --- list ---------------------------------------------------------------------

// TestScheduleListToolReturnsAndEmits verifies schedule_list renders the
// current table (id, kind, spec, state, next fire) and emits exactly one
// schedule/list event carrying the count (D3, serial tool path).
func TestScheduleListToolReturnsAndEmits(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	var recs []eventRecord
	tools := NewScheduleTools(e, collectEvents(&recs))

	if out, err := tools.List().Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("list empty: %v", err)
	} else if !strings.Contains(out, "no schedules") {
		t.Fatalf("empty list output = %q, want no schedules", out)
	}
	if _, err := tools.Create().Execute(context.Background(), json.RawMessage(`{"kind":"interval","spec":"1h"}`)); err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := tools.List().Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "sched-1") || !strings.Contains(out, "interval") || !strings.Contains(out, "enabled") {
		t.Fatalf("list output = %q, want the created schedule rendered", out)
	}
	if n := countEventType(recs, "schedule/list"); n != 2 {
		t.Fatalf("schedule/list count = %d, want 2", n)
	}
	var last struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(recs[len(recs)-1].raw), &last); err != nil {
		t.Fatalf("unmarshal schedule/list: %v", err)
	}
	if last.Count != 1 {
		t.Fatalf("schedule/list count = %d, want 1", last.Count)
	}
}

// --- delete -------------------------------------------------------------------

// TestScheduleDeleteToolRemovesAndEmits verifies schedule_delete removes a
// stored schedule, emits exactly one schedule/delete event with its id (D3,
// serial tool path), and rejects an unknown id.
func TestScheduleDeleteToolRemovesAndEmits(t *testing.T) {
	e := NewEngine(nil)
	defer e.Close()
	var recs []eventRecord
	tools := NewScheduleTools(e, collectEvents(&recs))

	s, err := tools.Create().Execute(context.Background(), json.RawMessage(`{"kind":"interval","spec":"30m"}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := strings.Fields(s)[2] // "created schedule sched-1 ..."
	out, err := tools.Delete().Execute(context.Background(), json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(out, "deleted schedule "+id) {
		t.Fatalf("delete output = %q, want deleted schedule %s", out, id)
	}
	if all, _ := e.List(context.Background()); len(all) != 0 {
		t.Fatalf("schedule %s still present after delete", id)
	}
	if n := countEventType(recs, "schedule/delete"); n != 1 {
		t.Fatalf("schedule/delete count = %d, want 1", n)
	}
	var del struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(recs[len(recs)-1].raw), &del); err != nil {
		t.Fatalf("unmarshal schedule/delete: %v", err)
	}
	if del.ID != id {
		t.Fatalf("schedule/delete id = %q, want %q", del.ID, id)
	}
	// An unknown id is rejected (ErrUnknownSchedule from the Engine/Provider).
	if _, err := tools.Delete().Execute(context.Background(), json.RawMessage(`{"id":"sched-99"}`)); err == nil {
		t.Fatal("schedule_delete of an unknown id must error")
	}
}
