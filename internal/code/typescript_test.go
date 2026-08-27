package code

import (
	"context"
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

func TestTypeScriptRuntimeAllowsIndependentBindingsToOverlap(t *testing.T) {
	runtime := NewTypeScriptRuntime()
	defer runtime.Close()

	var active, maxActive int32
	result, err := runtime.RunProgram(context.Background(), ProgramRequest{
		Code:      `const values = await Promise.all([tools.echo({ value: 1 }), tools.echo({ value: 2 })]); return values`,
		Cwd:       t.TempDir(),
		Timeout:   5 * time.Second,
		MaxOutput: 16 * 1024,
		Binding: func(_ context.Context, request ProgramBindingRequest) (any, error) {
			current := atomic.AddInt32(&active, 1)
			for previous := atomic.LoadInt32(&maxActive); current > previous && !atomic.CompareAndSwapInt32(&maxActive, previous, current); previous = atomic.LoadInt32(&maxActive) {
			}
			time.Sleep(400 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			return request.Args.(map[string]any)["value"], nil
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
	if result.Failure == nil || result.Failure.Kind != "budget" {
		t.Fatalf("timeout result = %+v, want budget failure", result)
	}
}

type testBindingError struct{ name string }

func (e *testBindingError) Error() string { return "unknown tool " + e.name }
