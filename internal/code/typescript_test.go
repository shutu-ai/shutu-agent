package code

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTypeScriptRuntimeExecutesProgramAndBridgesTools(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code:      `const value: number = await tools.echo({ value: 21 }); console.log('value', value); return { doubled: value * 2 }`,
		Cwd:       t.TempDir(),
		Timeout:   5 * time.Second,
		MaxOutput: 16 * 1024,
		Binding: func(_ context.Context, request ProgramBindingRequest) (any, error) {
			if request.Name != "echo" {
				t.Fatalf("binding name = %q, want echo", request.Name)
			}
			return request.Args.(map[string]any)["value"], nil
		},
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure != nil {
		t.Fatalf("program failure = %+v", *result.Failure)
	}
	if !result.HasValue || result.Value.(map[string]any)["doubled"] != float64(42) {
		t.Fatalf("program value = %#v", result.Value)
	}
	if len(result.Logs) != 1 || !strings.Contains(result.Logs[0], "value 21") {
		t.Fatalf("program logs = %#v", result.Logs)
	}
}

func TestTypeScriptRuntimeContainsHeapExhaustion(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		// Retain references so V8 cannot collect the allocations before the
		// configured old-generation ceiling is reached.
		Code: `const allocations: number[][] = []; while (true) allocations.push(new Array(1024 * 1024).fill(1))`,
		Cwd:  t.TempDir(), Timeout: 10 * time.Second, ComputeMS: 60_000,
		MaxWallMS: 10_000, MaxOutput: 16 * 1024, MaxOldGenerationSizeMB: 16,
		Binding: func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure == nil || result.Failure.Kind != ProgramFailureWorkerExit {
		t.Fatalf("heap exhaustion result = %+v, want worker-exit", result)
	}
}

func TestTypeScriptRuntimeTreatsUndefinedCompletionAsSuccess(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `console.log("finished")`, Cwd: t.TempDir(), Timeout: 5 * time.Second,
		MaxOutput: 16 * 1024, Binding: func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure != nil || result.HasValue {
		t.Fatalf("result = %+v, want successful completion without a value", result)
	}
	if len(result.Logs) != 1 || result.Logs[0] != "finished" {
		t.Fatalf("logs = %#v, want finished", result.Logs)
	}
}

func TestTypeScriptRuntimePreservesNullBindingResolution(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `return (await tools.nil({})) === null`, Cwd: t.TempDir(), Timeout: 5 * time.Second,
		MaxOutput: 16 * 1024, Binding: func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil || result.Failure != nil || !result.HasValue || result.Value != true {
		t.Fatalf("result = %+v, err=%v, want true after preserving null", result, err)
	}
}

func TestTypeScriptRuntimeTurnsLossyBindingResolutionIntoCatchableError(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `try { await tools.bad({}) } catch (error) { return { name: error.name, message: error.message } }`,
		Cwd:  t.TempDir(), Timeout: 5 * time.Second, MaxOutput: 16 * 1024,
		Binding: func(context.Context, ProgramBindingRequest) (any, error) {
			return func() {}, nil
		},
	})
	if err != nil || result.Failure != nil || !result.HasValue {
		t.Fatalf("result = %+v, err=%v, want catchable binding failure", result, err)
	}
	value := result.Value.(map[string]any)
	if value["name"] != "ToolCallError" || value["message"] != "binding resolution must be lossless JSON" {
		t.Fatalf("caught error = %#v", value)
	}
}

func TestTypeScriptRuntimeClassifiesSyntaxFailureAsProgramException(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `if (`, Cwd: t.TempDir(), Timeout: 5 * time.Second,
		MaxOutput: 16 * 1024, Binding: func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil || result.Failure == nil || result.Failure.Kind != ProgramFailureException {
		if result.Failure != nil {
			t.Fatalf("result = %+v failure=%+v, err=%v, want exception program failure", result, *result.Failure, err)
		}
		t.Fatalf("result = %+v, err=%v, want exception program failure", result, err)
	}
}

func TestTypeScriptRuntimeSupportsMultiplePortableNamespacesAndArbitraryMembers(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `return await files["read-file"]({ path: "a.txt" })`,
		Cwd:  t.TempDir(), Timeout: 5 * time.Second, MaxOutput: 16 * 1024,
		Bindings: []ProgramBindingNamespace{{Global: "tools"}, {Global: "files"}},
		Binding: func(_ context.Context, request ProgramBindingRequest) (any, error) {
			if request.Namespace != "files" || request.Name != "read-file" {
				t.Fatalf("binding identity = %q.%q", request.Namespace, request.Name)
			}
			return map[string]any{"ok": true}, nil
		},
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure != nil || !result.HasValue {
		t.Fatalf("program result = %+v", result)
	}
	value, ok := result.Value.(map[string]any)
	if !ok || value["ok"] != true {
		t.Fatalf("program value = %#v", result.Value)
	}
}

func TestTypeScriptRuntimeUsesNamespaceSpecificBindingErrorClass(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `try { await files["read-file"]({}) } catch (error) { return { name: error.name, member: error.memberName } }`,
		Cwd:  t.TempDir(), Timeout: 5 * time.Second, MaxOutput: 16 * 1024,
		Bindings: []ProgramBindingNamespace{{Global: "files", ErrorClassName: "FileBindingError", ErrorMemberNameProperty: "memberName"}},
		Binding: func(_ context.Context, request ProgramBindingRequest) (any, error) {
			if request.Namespace != "files" || request.Name != "read-file" {
				t.Fatalf("binding identity = %q.%q", request.Namespace, request.Name)
			}
			return nil, &testBindingError{name: request.Name}
		},
	})
	if err != nil || result.Failure != nil || !result.HasValue {
		t.Fatalf("program result=%+v err=%v", result, err)
	}
	value := result.Value.(map[string]any)
	if value["name"] != "FileBindingError" || value["member"] != "read-file" {
		t.Fatalf("namespace error = %#v", value)
	}
}

func TestTypeScriptRuntimeRejectsReservedBindingNamespaceBeforeStartingNode(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	_, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `return 1`, Cwd: t.TempDir(),
		Bindings: []ProgramBindingNamespace{{Global: "console"}},
		Binding:  func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "reserved binding namespace") {
		t.Fatalf("error = %v, want reserved namespace rejection", err)
	}
}

func TestTypeScriptRuntimeRejectsPortableReservedNamespaceAndErrorNames(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	for _, name := range []string{"arguments", "eval", "lambda", "nonlocal", "__name__", "__debug__"} {
		_, err := runtime.RunProgram(context.Background(), ProgramRequest{
			Code: `return 1`, Cwd: t.TempDir(),
			Bindings: []ProgramBindingNamespace{{Global: name}},
			Binding:  func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
		})
		if err == nil || !strings.Contains(err.Error(), "reserved binding namespace") {
			t.Errorf("namespace %q error = %v, want portable reserved-name rejection", name, err)
		}
	}
	for _, member := range []string{"name", "message", "stack", "args", "with_traceback", "add_note"} {
		_, err := runtime.RunProgram(context.Background(), ProgramRequest{
			Code: `return 1`, Cwd: t.TempDir(),
			Bindings: []ProgramBindingNamespace{{Global: "tools", ErrorClassName: "BindingError", ErrorMemberNameProperty: member}},
			Binding:  func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
		})
		if err == nil || !strings.Contains(err.Error(), "reserved binding error member property") {
			t.Errorf("error member %q error = %v, want cross-backend reserved-name rejection", member, err)
		}
	}
}

func TestTypeScriptRuntimePreservesCatchableToolErrors(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code:      `try { await tools.missing({}) } catch (error) { return { name: error.name, toolName: error.toolName, message: error.message } }`,
		Cwd:       t.TempDir(),
		Timeout:   5 * time.Second,
		MaxOutput: 16 * 1024,
		Binding: func(_ context.Context, request ProgramBindingRequest) (any, error) {
			return nil, &testBindingError{name: request.Name}
		},
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure != nil || !result.HasValue {
		t.Fatalf("program result = %+v failure=%+v", result, result.Failure)
	}
	value := result.Value.(map[string]any)
	if value["name"] != "ToolCallError" || value["toolName"] != "missing" || !strings.Contains(value["message"].(string), "missing") {
		t.Fatalf("caught error = %#v", value)
	}
}

func TestTypeScriptRuntimeSeparatesRichBindingValueFromProjection(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `return await tools.rich({})`, Cwd: t.TempDir(), Timeout: 5 * time.Second, MaxOutput: 16 * 1024,
		Binding: func(context.Context, ProgramBindingRequest) (any, error) {
			return ProgramBindingResult{Value: map[string]any{"answer": 42}, Content: []map[string]any{{"type": "image"}}}, nil
		},
	})
	if err != nil || result.Failure != nil || !result.HasValue {
		t.Fatalf("rich result=%+v err=%v", result, err)
	}
	value, ok := result.Value.(map[string]any)
	if !ok || value["answer"] != float64(42) {
		t.Fatalf("rich value=%#v, want only ProgramBindingResult.Value", result.Value)
	}
}

func TestTypeScriptRuntimeChargesCompletionAgainstOutputBudget(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `return "this completion is larger than the output budget"`, Cwd: t.TempDir(), Timeout: 5 * time.Second, MaxOutput: 8,
		Binding: func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failure == nil || result.Failure.Kind != "output-limit" || !result.Truncated {
		t.Fatalf("completion budget result=%+v, want output-limit/truncated", result)
	}
}

func TestTypeScriptRuntimeDrainsHostileRawStdoutFrame(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		// rawStdoutWrite is deliberately used here to bypass the framed console
		// shim. The host must cancel and drain an over-sized unterminated frame
		// instead of deadlocking on the OS pipe while waiting for Node.
		Code: `rawStdoutWrite("x".repeat(20 * 1024 * 1024)); return 1`, Cwd: t.TempDir(), Timeout: 5 * time.Second,
		MaxOutput: 16 * 1024, Binding: func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure == nil || result.Failure.Kind != ProgramFailureWorkerExit {
		t.Fatalf("hostile raw stdout result = %+v, want bounded worker-exit outcome", result)
	}
}

func TestTypeScriptRuntimeRejectsLossyBoundaryValues(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	var calls int32
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code: `
			const failures = [];
			for (const value of [new Date(), -0, (() => 1)]) {
				try { await tools.echo(value) } catch (error) { failures.push(error.message) }
			}
			return failures;
		`,
		Cwd:       t.TempDir(),
		Timeout:   5 * time.Second,
		MaxOutput: 16 * 1024,
		Binding: func(context.Context, ProgramBindingRequest) (any, error) {
			atomic.AddInt32(&calls, 1)
			return "unexpected", nil
		},
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure != nil || !result.HasValue {
		t.Fatalf("result = %+v, want catchable boundary errors", result)
	}
	if calls != 0 || len(result.Value.([]any)) != 3 {
		t.Fatalf("calls=%d value=%#v, want no host calls and three failures", calls, result.Value)
	}

	completion, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code:      `return new Date()`,
		Cwd:       t.TempDir(),
		Timeout:   5 * time.Second,
		MaxOutput: 16 * 1024,
		Binding:   func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("completion RunProgram: %v", err)
	}
	if completion.Failure == nil || completion.Failure.Kind != "invalid-output" {
		t.Fatalf("completion = %+v, want invalid-output", completion)
	}
}

func TestTypeScriptRuntimeAllowsIndependentBindingsToOverlap(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	var active, maxActive int32
	secondStarted := make(chan struct{})
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code:      `const values = await Promise.all([tools.echo({ value: 1 }), tools.echo({ value: 2 })]); return values`,
		Cwd:       t.TempDir(),
		Timeout:   5 * time.Second,
		MaxOutput: 16 * 1024,
		Binding: func(ctx context.Context, request ProgramBindingRequest) (any, error) {
			value := request.Args.(map[string]any)["value"]
			current := atomic.AddInt32(&active, 1)
			for previous := atomic.LoadInt32(&maxActive); current > previous && !atomic.CompareAndSwapInt32(&maxActive, previous, current); previous = atomic.LoadInt32(&maxActive) {
			}
			if value == float64(1) {
				select {
				case <-secondStarted:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			} else {
				close(secondStarted)
			}
			atomic.AddInt32(&active, -1)
			return value, nil
		},
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure != nil {
		t.Fatalf("program failure = %+v", *result.Failure)
	}
	if got := atomic.LoadInt32(&maxActive); got < 2 {
		t.Fatalf("independent bindings did not overlap: max active=%d", got)
	}
}

func TestTypeScriptRuntimeCancelsUnawaitedBindingsAfterProgramDone(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	started := make(chan struct{})
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code:      `void tools.slow({}); return "done"`,
		Cwd:       t.TempDir(),
		Timeout:   time.Second,
		MaxOutput: 16 * 1024,
		Binding: func(ctx context.Context, _ ProgramBindingRequest) (any, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("unawaited binding was not dispatched")
	}
	if result.Failure != nil || !result.HasValue || result.Value != "done" {
		t.Fatalf("result = %+v, want successful program after cancelling orphan binding", result)
	}
}

func TestProgramCallGateSerializesExclusiveCalls(t *testing.T) {
	gate := newProgramCallGate(2)
	ctx := context.Background()
	unsafe := make(chan struct{})
	if err := gate.acquire(ctx, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := gate.acquire(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	go func() {
		if err := gate.acquire(ctx, 2, false); err == nil {
			close(unsafe)
			gate.release(false)
		}
	}()
	select {
	case <-unsafe:
		t.Fatal("exclusive call started while safe calls were active")
	case <-time.After(50 * time.Millisecond):
	}
	gate.release(true)
	gate.release(true)
	waitForProgramGate(t, unsafe)
}

func waitForProgramGate(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("program call gate did not admit the expected call")
	}
}

func TestTypeScriptRuntimeTimeoutIsProgramFailure(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code:      `await new Promise(() => {})`,
		Cwd:       t.TempDir(),
		Timeout:   100 * time.Millisecond,
		MaxOutput: 1024,
		Binding:   func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure == nil || result.Failure.Kind != "timeout" {
		t.Fatalf("timeout result = %+v, want timeout failure", result)
	}
}

func TestTypeScriptRuntimeCancellationIsResolvedAbort(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		result ProgramResult
		err    error
	}, 1)
	go func() {
		result, err := runtime.RunProgram(ctx, ProgramRequest{
			Code: `await new Promise(resolve => setTimeout(resolve, 10_000)); return "late"`,
			Cwd:  t.TempDir(), Timeout: 30 * time.Second,
			Binding: func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
		})
		done <- struct {
			result ProgramResult
			err    error
		}{result: result, err: err}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("RunProgram returned transport error: %v", outcome.err)
		}
		if outcome.result.Failure == nil || outcome.result.Failure.Kind != "abort" {
			t.Fatalf("cancel result = %+v, want resolved abort failure", outcome.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled runtime did not settle")
	}
}

func TestTypeScriptRuntimeCloseCancelsHostBindingAndJoinsWorker(t *testing.T) {
	codeRuntime := NewTypeScriptRuntime()
	started := make(chan struct{})
	done := make(chan struct {
		result ProgramResult
		err    error
	}, 1)
	go func() {
		result, err := codeRuntime.RunProgram(context.Background(), ProgramRequest{
			Code: `await tools.wait({})`, Cwd: t.TempDir(), Timeout: 30 * time.Second,
			Binding: func(ctx context.Context, _ ProgramBindingRequest) (any, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			},
		})
		done <- struct {
			result ProgramResult
			err    error
		}{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("host binding did not start")
	}
	if err := codeRuntime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("RunProgram returned transport error after Close: %v", outcome.err)
		}
		if outcome.result.Failure == nil {
			t.Fatalf("RunProgram result after Close = %+v, want a resolved failure", outcome.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join the active worker and host binding")
	}
}

func TestTypeScriptRuntimeUsesEmptyEnvironment(t *testing.T) {
	codeRuntime := NewTypeScriptRuntime()
	defer codeRuntime.Close()

	result, err := codeRuntime.RunProgram(context.Background(), ProgramRequest{
		Code: `return JSON.stringify(process.env)`, Cwd: t.TempDir(), Timeout: 5 * time.Second,
		MaxOutput: 16 * 1024, Binding: func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("RunProgram: %v", err)
	}
	if result.Failure != nil || !result.HasValue {
		t.Fatalf("result = %+v, want successful environment value", result)
	}
	var env map[string]string
	encoded, ok := result.Value.(string)
	if !ok || json.Unmarshal([]byte(encoded), &env) != nil {
		t.Fatalf("environment value = %#v, want JSON object", result.Value)
	}
	if runtime.GOOS == "windows" {
		delete(env, "SYSTEMROOT")
	}
	if len(env) != 0 {
		t.Fatalf("Code Runtime environment = %#v, want empty apart from OS prerequisites", env)
	}
}

func TestTypeScriptRuntimeDeniesAmbientWorkerEffects(t *testing.T) {
	codeRuntime := NewTypeScriptRuntime()
	defer codeRuntime.Close()

	result, err := codeRuntime.RunProgram(context.Background(), ProgramRequest{
		Code: `return {
			readSelf: process.permission.has("fs.read", process.execPath),
			writeCwd: process.permission.has("fs.write", process.cwd()),
			child: process.permission.has("child").toString(),
			worker: process.permission.has("worker").toString(),
		}`,
		Cwd: t.TempDir(), Timeout: 5 * time.Second, MaxOutput: 16 * 1024,
		Binding: func(context.Context, ProgramBindingRequest) (any, error) { return nil, nil },
	})
	if err != nil || result.Failure != nil || !result.HasValue {
		t.Fatalf("result = %+v, err=%v, want permission report", result, err)
	}
	value, ok := result.Value.(map[string]any)
	if !ok {
		t.Fatalf("permission value = %#v, want object", result.Value)
	}
	if value["readSelf"] != false || value["writeCwd"] != false || value["child"] != "false" || value["worker"] != "false" {
		t.Fatalf("ambient worker permissions = %#v, want all denied", value)
	}
}

type testBindingError struct{ name string }

func (e *testBindingError) Error() string { return "unknown tool " + e.name }
