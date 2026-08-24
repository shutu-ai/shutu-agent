package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// makeFsApp builds a minimal app for fs wiring tests: only the fields
// registerFs touches (cfg.Fs, reg, log) are set.
func makeFsApp(fsEnabled bool, root string) *app {
	return &app{
		cfg: config.Config{
			Fs: config.FsConfig{Enabled: config.Bool(fsEnabled), Root: root},
		},
		reg: tools.New(),
		log: session.New(),
	}
}

// fsPolicy whitelists the fs seam tools so the registry Execute gate can run
// them (in production config.applyDefaults + PolicyFromConfig do this).
func fsPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"read", "write", "list", "edit"},
		Timeout:     0,
		OutputLimit: 0,
	}
}

// TestRegisterFsDisabledRegistersNothing verifies the D10 gate: with
// fs.enabled=false the composition root creates no FileService and registers
// no fs seam tool (dispatch-m6f-3 §5 / 自测: enabled=false 不注册).
func TestRegisterFsDisabledRegistersNothing(t *testing.T) {
	a := makeFsApp(false, "")
	if err := a.registerFs(); err != nil {
		t.Fatalf("registerFs: %v", err)
	}
	if a.fs != nil {
		t.Fatal("fs FileService must be nil when fs.enabled=false")
	}
	for _, spec := range a.reg.Specs() {
		switch spec.Name {
		case "write", "list", "edit":
			t.Fatalf("%s registered while fs disabled", spec.Name)
		}
	}
}

// TestRegisterFsEnabledRegistersAndValidates verifies the enabled path: the
// local FileService is created (root pinned to the test dir), the write /
// list / edit tools are registered, D7 rejects bad arguments at the Execute
// gate, a valid write/list/edit flow through and land fs/write and fs/list in
// the session log (D3) without deriving into history (log-only), and an
// out-of-bounds path returns an error message.
func TestRegisterFsEnabledRegistersAndValidates(t *testing.T) {
	root := t.TempDir()
	a := makeFsApp(true, root)
	a.reg.SetPolicy(fsPolicy())
	if err := a.registerFs(); err != nil {
		t.Fatalf("registerFs: %v", err)
	}
	defer a.fs.Close()
	if a.fs == nil {
		t.Fatal("fs FileService must be created when fs.enabled=true")
	}
	if a.fs.Root() != root {
		t.Fatalf("fs root = %q, want %q", a.fs.Root(), root)
	}
	found := map[string]bool{}
	for _, s := range a.reg.Specs() {
		found[s.Name] = true
	}
	for _, name := range []string{"read", "write", "list", "edit"} {
		if !found[name] {
			t.Fatalf("%s not registered when fs.enabled=true", name)
		}
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct{ name, args string }{
		{"write", `{}`},                               // missing required path/content
		{"write", `{"path":"x.txt"}`},                 // missing required content
		{"write", `{"path":"x.txt","content":123}`},   // content must be a string
		{"write", `{"path":"x","content":"x","e":1}`}, // additional properties rejected
		{"list", `{}`},                                // missing required dir
		{"list", `{"dir":1}`},                         // dir must be a string
		{"list", `{"dir":"x","extra":1}`},             // additional properties rejected
		{"edit", `{}`},                                // missing required fields
		{"edit", `{"path":"x","old_string":"a"}`},     // missing new_string
		{"edit", `{"path":"x","old_string":"a","new_string":"b","e":1}`}, // additional properties rejected
	} {
		if _, err := a.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// A valid write/list/edit flow through the registry.
	if _, err := a.reg.Execute(context.Background(), "write", json.RawMessage(`{"path":"notes.txt","content":"hello fs"}`)); err != nil {
		t.Fatalf("write via registry: %v", err)
	}
	if !hasEvent(a.log, session.EventFsWrite) {
		t.Fatal("fs/write event missing from the session log after write")
	}
	res2, err := a.reg.Execute(context.Background(), "list", json.RawMessage(`{"dir":"."}`))
	if err != nil {
		t.Fatalf("list via registry: %v", err)
	}
	if !strings.Contains(res2.Output, "notes.txt") {
		t.Fatalf("list output = %q, want it to list notes.txt", res2.Output)
	}
	if !hasEvent(a.log, session.EventFsList) {
		t.Fatal("fs/list event missing from the session log after list")
	}
	if _, err := a.reg.Execute(context.Background(), "read", json.RawMessage(`{"path":"notes.txt"}`)); err != nil {
		t.Fatalf("read via registry: %v", err)
	}
	if _, err := a.reg.Execute(context.Background(), "edit", json.RawMessage(`{"path":"notes.txt","old_string":"hello","new_string":"edited"}`)); err != nil {
		t.Fatalf("edit via registry: %v", err)
	}
	// The events are log-only: nothing derives into model-visible messages.
	if msgs := a.log.DeriveHistory(); len(msgs) != 0 {
		t.Fatalf("fs/* events must not derive into messages: %+v", msgs)
	}

	// An out-of-bounds path returns an error message, never a panic.
	before := len(a.log.Events())
	if _, err := a.reg.Execute(context.Background(), "write", json.RawMessage(`{"path":"../../x.txt","content":"x"}`)); err == nil {
		t.Fatal("write of an escaping path must error at the registry")
	}
	if _, err := a.reg.Execute(context.Background(), "edit", json.RawMessage(`{"path":"../../x.txt","old_string":"a","new_string":"b"}`)); err == nil {
		t.Fatal("edit of an escaping path must error at the registry")
	}
	if after := len(a.log.Events()); after != before {
		t.Fatalf("failed write/edit logged %d events, want none (only successes log fs/* facts)", after-before)
	}
	// The rejected write must not have created anything outside the root.
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "x.txt")); !os.IsNotExist(err) {
		t.Fatalf("rejected write must not create %s", filepath.Join(filepath.Dir(root), "x.txt"))
	}
}
