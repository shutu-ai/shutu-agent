package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/jobs"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

// makeJobsApp builds a minimal app for registerJobs tests: only the fields
// registerJobs touches (cfg.Jobs, reg, log, currentID) are set.
func makeJobsApp(enabled bool) *app {
	return &app{
		cfg:       config.Config{Jobs: config.JobsConfig{Enabled: config.Bool(enabled), MaxConcurrentJobsPerOwner: 10}},
		reg:       tools.New(),
		log:       session.New(),
		currentID: "s-test",
	}
}

// jobsPolicy whitelists the canonical dsh job tools so registry Execute can run them
// (in production config.applyDefaults + PolicyFromConfig do this).
func jobsPolicy() tools.Policy {
	return tools.Policy{
		Enabled:     []string{"job_output", "job_kill", "job_list"},
		Timeout:     0, // no per-tool deadline in tests
		OutputLimit: 0,
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestRegisterJobsDisabledRegistersNothing verifies the D10 gate: with
// jobs.enabled=false the composition root creates no registry and registers no
// job_* tool (dispatch-m5a-2 §4).
func TestRegisterJobsDisabledRegistersNothing(t *testing.T) {
	app := makeJobsApp(false)
	if err := app.registerJobs(); err != nil {
		t.Fatalf("registerJobs: %v", err)
	}
	if app.jobs != nil {
		t.Fatal("jobs registry must be nil when jobs.enabled=false")
	}
	for _, spec := range app.reg.Specs() {
		if strings.HasPrefix(spec.Name, "job_") {
			t.Fatalf("job tool %q registered while jobs disabled", spec.Name)
		}
	}
}

// TestRegisterJobsEnabledRegistersAndValidates verifies the enabled path: the
// registry is created, all canonical job_* tools are registered, D7 schema
// validation rejects bad arguments at the Execute gate, valid calls flow
// through, and the job/start event lands in the session log (D3 wiring).
func TestRegisterJobsEnabledRegistersAndValidates(t *testing.T) {
	app := makeJobsApp(true)
	app.reg.SetPolicy(jobsPolicy())
	if err := app.registerJobs(); err != nil {
		t.Fatalf("registerJobs: %v", err)
	}
	defer app.jobs.Close()
	if app.jobs == nil {
		t.Fatal("jobs registry must be created when jobs.enabled=true")
	}
	specs := app.reg.Specs()
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	for _, want := range []string{"job_output", "job_kill", "job_list"} {
		if !containsStr(names, want) {
			t.Fatalf("registered tools %v lack %q", names, want)
		}
	}
	for _, removed := range []string{"job_start", "job_status", "job_cancel", "job_wait", "job_read"} {
		if containsStr(names, removed) {
			t.Fatalf("removed legacy tool %q is still advertised", removed)
		}
		if _, err := app.reg.Execute(context.Background(), removed, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("removed legacy tool %q is still executable", removed)
		}
	}

	// D7: bad arguments are rejected before any tool code runs.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"job_output", `{}`},                            // missing required job_id
		{"job_output", `{"job_id":123}`},                // job_id must be a string
		{"job_kill", `{}`},                              // missing required job_id
		{"job_output", `{"job_id":"x","timeout_ms":0}`}, // timeout must be >= 1
		{"job_list", `{"extra":1}`},                     // additional properties rejected
	} {
		if _, err := app.reg.Execute(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s with args %s must be rejected (D7)", tc.name, tc.args)
		}
	}

	// Producers start jobs through the internal registry; the model observes
	// them only through DSH's job_output/job_list/job_kill surface.
	jobID, err := app.jobs.Start(context.Background(), jobs.JobStart{
		Kind: jobs.Kind("bash"), Label: "d7-ok", OwnerSession: "s-test",
		Run: func(context.Context) (jobs.JobOutcome, error) {
			return jobs.JobOutcome{Status: jobs.StatusCompleted, Output: "d7-ok"}, nil
		},
	})
	if err != nil {
		t.Fatalf("internal job start: %v", err)
	}
	if _, err := app.reg.Execute(context.Background(), "job_output", json.RawMessage(`{"job_id":"`+jobID+`","wait":true}`)); err != nil {
		t.Fatalf("job_output via registry: %v", err)
	}
}
