package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type referenceACPMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type referenceACPProcess struct {
	stdin      io.WriteCloser
	messages   chan referenceACPMessage
	stdoutDone chan struct{}
	waitDone   chan error
	waitOnce   sync.Once
	waitErr    error
	stderr     *synchronizedBuffer
	cmd        *exec.Cmd
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func writeReferenceACPFiles(t *testing.T, directory, referenceRoot string) error {
	t.Helper()

	loaderPath := filepath.Join(directory, "reference-source-loader.mjs")
	loader, err := os.ReadFile(filepath.Join("testdata", "reference_source_loader.mjs"))
	if err != nil {
		return err
	}
	loaderText := strings.ReplaceAll(
		string(loader),
		"process.env.DSH_REFERENCE_ROOT",
		fmtReferencePathLiteral(referenceRoot),
	)
	if err := os.WriteFile(loaderPath, []byte(loaderText), 0o600); err != nil {
		return err
	}

	helper := `import { Readable, Writable } from 'node:stream'
import { Context } from '@deepseek-ai/cordis'
import { ndJsonStream } from '@agentclientprotocol/sdk'
import { LlmAdapter } from '@deepseek-ai/dsh-llm'
import AgentLoop from '@deepseek-ai/dsh-agent-loop'
import { mountAgentLoopTestDependencies } from '@deepseek-ai/dsh-agent-loop-testkit'
import * as AcpPlugin from '@deepseek-ai/dsh-acp'

class ReferenceReplayAdapter extends LlmAdapter {
  async *stream() {
    const text = 'reference runtime replay'
    yield { type: 'block-start', index: 0, blockType: 'text' }
    yield { type: 'text-delta', index: 0, text }
    yield { type: 'block-end', index: 0, block: { type: 'text', text } }
    yield { type: 'usage', usage: { inputTokens: 1, outputTokens: text.length } }
    yield { type: 'finish', reason: { kind: 'stop' } }
  }
}

const context = new Context()
await mountAgentLoopTestDependencies(context, { systemPrompt: { persona: '' } })
await context.plugin(AgentLoop, { agents: [] })
context.llm.registerAdapter(['mock'], new ReferenceReplayAdapter())

const stream = ndJsonStream(
  Writable.toWeb(process.stdout),
  Readable.toWeb(process.stdin),
)
await context.plugin({
  name: 'reference-acp-runtime-test',
  inject: [...AcpPlugin.inject],
  apply: inner => AcpPlugin.apply(inner, {
    provider: 'mock',
    model: 'mock',
    stream,
  }),
})

let closing = false
process.stdin.once('end', async () => {
  if (closing) return
  closing = true
  try {
    await context.fiber.dispose()
    process.exit(0)
  } catch (error) {
    console.error(error)
    process.exit(1)
  }
})
`
	return os.WriteFile(filepath.Join(directory, "reference-acp-helper.ts"), []byte(helper), 0o600)
}

func startReferenceACPRuntime(t *testing.T, root, referenceRoot string) *referenceACPProcess {
	t.Helper()

	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("Node runtime is unavailable: %v", err)
	}
	cmd := exec.Command("node",
		"--experimental-transform-types",
		"--loader", fileURLForWindowsPath(filepath.Join(root, "reference-source-loader.mjs")),
		filepath.Join(root, "reference-acp-helper.ts"),
	)
	cmd.Dir = referenceRoot
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}

	stderr := &synchronizedBuffer{}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderr, stderrPipe)
	}()

	process := &referenceACPProcess{
		stdin:      stdin,
		messages:   make(chan referenceACPMessage, 16),
		stdoutDone: make(chan struct{}),
		waitDone:   make(chan error, 1),
		stderr:     stderr,
		cmd:        cmd,
	}
	go func() {
		defer close(process.stdoutDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var message referenceACPMessage
			if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
				t.Errorf("reference ACP emitted invalid ndjson: %v: %q", err, scanner.Text())
				return
			}
			select {
			case process.messages <- message:
			case <-time.After(15 * time.Second):
				t.Error("reference ACP message channel timed out")
				return
			}
		}
		if err := scanner.Err(); err != nil {
			t.Errorf("read reference ACP stdout: %v", err)
		}
	}()

	if err := cmd.Start(); err != nil {
		t.Fatalf("start pinned reference ACP runtime: %v", err)
	}
	go func() { process.waitDone <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() {
			_ = process.wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		<-process.stdoutDone
		<-stderrDone
	})
	return process
}

func (p *referenceACPProcess) wait() error {
	p.waitOnce.Do(func() { p.waitErr = <-p.waitDone })
	return p.waitErr
}

func (p *referenceACPProcess) nextMessage(t *testing.T, timeout time.Duration) referenceACPMessage {
	t.Helper()
	select {
	case message, ok := <-p.messages:
		if !ok {
			<-p.waitDone
			t.Fatalf("reference ACP stdout closed; stderr: %s", strings.TrimSpace(p.stderr.String()))
		}
		return message
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		t.Fatalf("timed out waiting for reference ACP message; stderr: %s", strings.TrimSpace(p.stderr.String()))
	}
	return referenceACPMessage{}
}

func (p *referenceACPProcess) write(t *testing.T, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if _, err := p.stdin.Write(encoded); err != nil {
		t.Fatalf("write reference ACP request: %v", err)
	}
}

func (p *referenceACPProcess) waitForID(t *testing.T, id string, timeout time.Duration) referenceACPMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		message := p.nextMessage(t, time.Until(deadline))
		if len(message.ID) > 0 && string(message.ID) == id {
			return message
		}
	}
}

func TestPinnedReferenceACPRuntimeExternalClient(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	referenceRoot := filepath.Clean(filepath.Join(repoRoot, "..", "deepseek-harness"))
	if envRoot := strings.TrimSpace(os.Getenv("DSH_REFERENCE_ROOT")); envRoot != "" {
		referenceRoot = envRoot
	}
	for _, relative := range []string{
		"packages/acp/acp/src/index.ts",
		"packages/core/agent-loop/src/index.ts",
		"packages/test-support/agent-loop-testkit/src/index.ts",
		"node_modules/@agentclientprotocol/sdk/dist/acp.js",
		"node_modules/typescript/lib/typescript.js",
	} {
		if _, err := os.Stat(filepath.Join(referenceRoot, filepath.FromSlash(relative))); err != nil {
			t.Skipf("pinned reference checkout is incomplete: %v", err)
		}
	}

	root := t.TempDir()
	if err := writeReferenceACPFiles(t, root, referenceRoot); err != nil {
		t.Fatal(err)
	}
	process := startReferenceACPRuntime(t, root, referenceRoot)

	process.write(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion":    1,
			"clientCapabilities": map[string]any{},
		},
	})
	var initialize struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
	}
	if err := json.Unmarshal(process.waitForID(t, "1", 20*time.Second).Result, &initialize); err != nil {
		t.Fatalf("decode reference initialize: %v", err)
	}
	if initialize.ProtocolVersion != 1 ||
		initialize.AgentInfo.Name != "deepseek-harness-acp" ||
		initialize.AgentInfo.Version != "0.0.1" {
		t.Fatalf("reference ACP identity = %#v", initialize)
	}

	process.write(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "session/new",
		"params": map[string]any{
			"cwd":        root,
			"mcpServers": []any{},
		},
	})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(process.waitForID(t, "2", 20*time.Second).Result, &created); err != nil {
		t.Fatalf("decode reference session/new: %v", err)
	}
	if created.SessionID == "" {
		t.Fatal("reference session/new returned an empty session id")
	}

	process.write(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": created.SessionID,
			"prompt": []map[string]any{{
				"type": "text",
				"text": "reference runtime replay",
			}},
		},
	})

	var sawAssistantChunk, sawEndTurn bool
	deadline := time.Now().Add(20 * time.Second)
	for !sawAssistantChunk || !sawEndTurn {
		message := process.nextMessage(t, time.Until(deadline))
		if message.Method == "session/update" {
			var update struct {
				SessionID string `json:"sessionId"`
				Update    struct {
					SessionUpdate string `json:"sessionUpdate"`
					Content       struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			}
			if err := json.Unmarshal(message.Params, &update); err != nil {
				t.Fatalf("decode reference session/update: %v", err)
			}
			if update.SessionID == created.SessionID &&
				update.Update.SessionUpdate == "agent_message_chunk" &&
				update.Update.Content.Type == "text" &&
				update.Update.Content.Text == "reference runtime replay" {
				sawAssistantChunk = true
			}
			continue
		}
		if len(message.ID) > 0 && string(message.ID) == "3" {
			if message.Error != nil {
				t.Fatalf("reference prompt failed: %d %s", message.Error.Code, message.Error.Message)
			}
			var result struct {
				StopReason string `json:"stopReason"`
			}
			if err := json.Unmarshal(message.Result, &result); err != nil {
				t.Fatalf("decode reference prompt result: %v", err)
			}
			sawEndTurn = result.StopReason == "end_turn"
		}
	}

	if err := process.stdin.Close(); err != nil {
		t.Fatalf("close reference ACP client stdin: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- process.wait() }()
	select {
	case waitErr := <-waitCh:
		if waitErr != nil {
			t.Fatalf("reference ACP runtime did not exit cleanly: %v; stderr: %s",
				waitErr, strings.TrimSpace(process.stderr.String()))
		}
		if code := process.cmd.ProcessState.ExitCode(); code != 0 {
			t.Fatalf("reference ACP runtime exit code = %d; stderr: %s", code, process.stderr.String())
		}
	case <-time.After(15 * time.Second):
		_ = process.cmd.Process.Kill()
		t.Fatalf("reference ACP runtime remained live after client EOF; stderr: %s", process.stderr.String())
	}
	<-process.stdoutDone
}
