package code

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func assertNoProgramTempFiles(t *testing.T, cwd string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(cwd, ".shutu-ptc-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("Code Mode left %d generated program files: %v", len(leftovers), leftovers)
	}
}

func TestTypeScriptRuntimeTimeoutLeavesNoProgramTempFile(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	cwd := t.TempDir()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code:      `await new Promise(() => {})`,
		Cwd:       cwd,
		Timeout:   100 * time.Millisecond,
		MaxOutput: 16 * 1024,
		Binding:   func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure == nil || result.Failure.Kind != ProgramFailureTimeout {
		t.Fatalf("timeout result = %+v, want one terminal timeout receipt", result)
	}
	assertNoProgramTempFiles(t, cwd)
}

func TestTypeScriptRuntimeWorkerExitLeavesNoProgramTempFile(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	cwd := t.TempDir()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code:      `rawStdoutWrite("x".repeat(20 * 1024 * 1024)); return "must-not-settle"`,
		Cwd:       cwd,
		Timeout:   5 * time.Second,
		MaxOutput: 16 * 1024,
		Binding:   func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure == nil || result.Failure.Kind != ProgramFailureWorkerExit {
		t.Fatalf("worker-exit result = %+v, want one terminal worker-exit receipt", result)
	}
	assertNoProgramTempFiles(t, cwd)
}

func TestTypeScriptRuntimeAdmitsForgedDuplicateCallExactlyOnce(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	cwd := t.TempDir()

	var calls atomic.Int32
	started := make(chan struct{})
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `
			const frame = JSON.stringify({
				type: 'call', id: 424242, namespace: 'tools', name: 'effect', args: {},
			}) + '\n'
			rawStdoutWrite(frame + frame)
			await new Promise(resolve => setTimeout(resolve, 25))
			return 'done'
		`,
		Cwd:       cwd,
		Timeout:   5 * time.Second,
		MaxOutput: 16 * 1024,
		Binding: func(_ context.Context, request ProgramBindingRequest) (any, error) {
			if request.CallID != "code:424242" || request.Name != "effect" {
				t.Errorf("unexpected binding request = %#v", request)
			}
			if calls.Add(1) == 1 {
				close(started)
			}
			return "committed", nil
		},
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure != nil || !result.HasValue || result.Value != "done" {
		t.Fatalf("result = %+v, want successful completion after duplicate admission", result)
	}
	select {
	case <-started:
	default:
		t.Fatal("host binding did not start")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("forged duplicate call admitted %d host effects, want one", got)
	}
	if strings.Contains(strings.Join(result.Logs, "\n"), "must-not") {
		t.Fatalf("unexpected hostile logs = %#v", result.Logs)
	}
	assertNoProgramTempFiles(t, cwd)
}

func TestTypeScriptRuntimeAbandonsQueuedCallAtSettlement(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	cwd := t.TempDir()

	var calls atomic.Int32
	started := make(chan struct{})
	var startOnce sync.Once
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `
			const first = tools.effect({ id: 'in-flight' }).catch(() => 'first-settled')
			const second = tools.effect({ id: 'queued' }).catch(error => {
				console.log('ABANDONED:' + error.message)
				return 'queued-settled'
			})
			await new Promise(resolve => setTimeout(resolve, 100))
			throw new Error('program failed with a queued call')
		`,
		Cwd:                 cwd,
		Timeout:             5 * time.Second,
		MaxOutput:           16 * 1024,
		MaxParallelSubCalls: 1,
		IsConcurrencySafe: func(_ string, args any) bool {
			value, _ := args.(map[string]any)
			return value["id"] != "in-flight"
		},
		Binding: func(callCtx context.Context, request ProgramBindingRequest) (any, error) {
			if request.Name != "effect" || request.Args.(map[string]any)["id"] != "in-flight" {
				calls.Add(1)
				t.Errorf("unexpected binding request = %#v", request)
				return nil, errors.New("unexpected queued binding")
			}
			startOnce.Do(func() { close(started) })
			<-callCtx.Done()
			return nil, callCtx.Err()
		},
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("in-flight host binding did not start")
	}
	if result.Failure == nil || result.Failure.Kind != ProgramFailureException ||
		!strings.Contains(result.Failure.Message, "program failed with a queued call") {
		if result.Failure != nil {
			t.Fatalf("result failure = %#v, want one terminal program exception", *result.Failure)
		}
		t.Fatalf("result = %#v, want one terminal program exception", result)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("queued call reached %d unexpected host bindings, want zero", got)
	}
	if logs := strings.Join(result.Logs, "\n"); !strings.Contains(logs,
		"ABANDONED:run_code run is over (run_code settled); effect tool call abandoned") {
		t.Fatalf("queued worker promise = %q, want explicit abandonment", logs)
	}
	assertNoProgramTempFiles(t, cwd)
}

func TestTypeScriptRuntimeDeniesRealAmbientProcessAndWorkerEffects(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	cwd := t.TempDir()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `
			const outcomes: Record<string, boolean> = {}
			try {
				const childProcess = await import('node:child_process')
				const child = childProcess.spawn('true')
				child.unref()
				outcomes.child = false
			} catch {
				outcomes.child = true
			}
			try {
				const workerThreads = await import('node:worker_threads')
				const worker = new workerThreads.Worker('./denied-by-permission-model.js')
				await worker.terminate()
				outcomes.worker = false
			} catch {
				outcomes.worker = true
			}
			return outcomes
		`,
		Cwd:       cwd,
		Timeout:   5 * time.Second,
		MaxOutput: 16 * 1024,
		Binding:   func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure != nil || !result.HasValue {
		t.Fatalf("result = %+v, want permission report", result)
	}
	value, ok := result.Value.(map[string]any)
	if !ok {
		t.Fatalf("permission report = %#v, want object", result.Value)
	}
	if value["child"] != true || value["worker"] != true {
		t.Fatalf("real ambient effect admission = %#v, want child and worker denied", value)
	}
	assertNoProgramTempFiles(t, cwd)
}
