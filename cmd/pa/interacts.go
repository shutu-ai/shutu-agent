// interacts.go — the M6d-2 composition-root orchestration (dispatch-m6d-2
// §4/§5). This is where the human-approval capability seam is wired into the
// REPL: registerInteracts creates the in-memory Provider + Engine, registers
// the two interact_* tools and installs the sensitive-tool gate on the tool
// registry when interact.enabled (D10), and wires the D3 event sink so
// interact/* events are appended to the active session log.
//
// The sensitive-tool gate is the ADR 决策 M6d 落地 (design.md §10 D5): when
// interact.sensitive_tools is non-empty and the model requests a gated tool,
// the gate first Engine.Requests a human approval, then blocks on the CLI
// serial path reading the user's y/n answer from the terminal, then records the
// decision (Engine.Resolve) and re-reads it through Engine.Await (the
// caller-driven poll — the resolution made here is visible on the next probe).
// Approved lets the tool run; rejected returns a denial the model sees as a
// tool/error. The loop's turn/step structure is untouched (D4): the gate hangs
// off the tools registry's pre-execution hook (tools.AddPreExecuteHook), not the loop.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jabing/shutu-agent/internal/config"
	"github.com/jabing/shutu-agent/internal/interact"
	"github.com/jabing/shutu-agent/internal/session"
	"github.com/jabing/shutu-agent/internal/tools"
)

type webApprovalContextKey struct{}

func withWebApprovalContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, webApprovalContextKey{}, true)
}

func isWebApprovalContext(ctx context.Context) bool {
	value, _ := ctx.Value(webApprovalContextKey{}).(bool)
	return value
}

// approveArgsBound mirrors the approval engine's stored-args bound (interact
// maxArgsLen, 200 runes): the gate trims an over-long tool args payload to it
// so the approval Request can never fail on the gate path.
const approveArgsBound = 200

// registerInteracts creates the in-memory Provider + Engine, registers the two
// interact_* tools and installs the sensitive-tool gate when interact.enabled,
// and wires the D3 event sink. When interact is disabled it creates nothing and
// registers nothing (D10, mirrors registerJobs/registerSkills). It must run
// after every other register* so the sensitive-tool gate can see the full
// registered tool set (it is called last in main.go).
func (a *app) registerInteracts() error {
	if !config.Enabled(a.cfg.Interact.Enabled) {
		return nil
	}
	prov := interact.NewMemProvider()
	eng := interact.NewEngine(prov)
	a.interacts = eng
	if a.store != nil {
		if err := a.restoreInteractions(context.Background(), eng); err != nil {
			return fmt.Errorf("pa: restore interactions: %w", err)
		}
	}
	// D3 event sink: interact/* events are appended to the active session log.
	// The callback only ever runs inside an interact_* tool Execute or the
	// sensitive-tool gate — the serial main-loop path (D5). a.log is read at
	// call time, so a session switch (/new, /resume) is honored the same way
	// as the other session-bound event wiring.
	onEvent := func(typ string, data any) {
		if typ == session.EventInteractRequest {
			var request struct {
				ID string `json:"id"`
			}
			if raw, err := json.Marshal(data); err == nil {
				_ = json.Unmarshal(raw, &request)
			}
			if request.ID != "" {
				a.interactionMu.Lock()
				if a.interactionSessions == nil {
					a.interactionSessions = make(map[string]string)
				}
				a.interactionSessions[request.ID] = a.currentID
				a.interactionMu.Unlock()
			}
		}
		if _, err := a.log.Append(typ, data); err != nil {
			fmt.Fprintln(os.Stderr, "pa: "+typ+" event:", err)
		}
	}
	st := interact.NewInteractTools(eng, onEvent)
	for _, t := range []tools.Tool{st.AskUserQuestion()} {
		if err := a.reg.Register(t); err != nil {
			return fmt.Errorf("pa: register %s: %w", t.Name(), err)
		}
	}
	// Sensitive-tool gate: install the registry pre-execution gate when
	// sensitive_tools is non-empty (an enabled interact with an empty list
	// registers only the interact_* tools — no gating).
	if len(a.cfg.Interact.SensitiveTools) > 0 {
		gate := a.sensitiveGate(eng, onEvent)
		a.reg.AddPreExecuteHook(func(ctx context.Context, exec tools.Execution) (tools.PreToolDecision, error) {
			if err := gate(ctx, exec.Name, exec.Arguments); err != nil {
				return tools.PreToolDecision{Kind: "deny", Reason: err.Error()}, nil
			}
			return tools.PreToolDecision{Kind: "allow"}, nil
		})
	}
	return nil
}

// restoreInteractions rebuilds the process-local approval table and its
// session ownership index from durable interact/request + interact/resolve
// facts. Old request events without detail remain visible with a safe fallback
// prompt, while newer events restore the full approval card.
func (a *app) restoreInteractions(ctx context.Context, restorer interface {
	Restore(context.Context, []interact.Request) error
}) error {
	metas, err := a.store.ListSessions(ctx)
	if err != nil {
		return err
	}
	type requestFact struct {
		ID        string              `json:"id"`
		ToolName  string              `json:"toolName"`
		Prompt    string              `json:"prompt"`
		Args      string              `json:"args"`
		Questions []interact.Question `json:"questions"`
	}
	type resolveFact struct {
		ID       string `json:"id"`
		Approved bool   `json:"approved"`
	}
	requests := make(map[string]interact.Request)
	owners := make(map[string]string)
	for _, meta := range metas {
		events, err := a.store.LoadSession(ctx, meta.ID)
		if err != nil {
			return err
		}
		for _, ev := range events {
			switch ev.Type {
			case session.EventInteractRequest:
				var fact requestFact
				if err := json.Unmarshal(ev.Data, &fact); err != nil || fact.ID == "" {
					continue
				}
				prompt := fact.Prompt
				if prompt == "" {
					prompt = fmt.Sprintf("Approval required for %s", fact.ToolName)
				}
				requests[fact.ID] = interact.Request{
					ID: fact.ID, Prompt: prompt, ToolName: fact.ToolName, Args: fact.Args,
					Questions: fact.Questions, Status: interact.StatusPending, CreatedAt: ev.At,
				}
				owners[fact.ID] = meta.ID
			case session.EventInteractResolve:
				var fact resolveFact
				if err := json.Unmarshal(ev.Data, &fact); err != nil || fact.ID == "" {
					continue
				}
				r, ok := requests[fact.ID]
				if !ok {
					continue
				}
				if fact.Approved {
					r.Status = interact.StatusApproved
				} else {
					r.Status = interact.StatusRejected
				}
				resolvedAt := ev.At
				r.ResolvedAt = &resolvedAt
				requests[fact.ID] = r
			case session.EventInteractCancel:
				var fact struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(ev.Data, &fact); err != nil || fact.ID == "" {
					continue
				}
				r, ok := requests[fact.ID]
				if !ok {
					continue
				}
				r.Status = interact.StatusCanceled
				resolvedAt := ev.At
				r.ResolvedAt = &resolvedAt
				requests[fact.ID] = r
			}
		}
	}
	restored := make([]interact.Request, 0, len(requests))
	for _, request := range requests {
		restored = append(restored, request)
	}
	if err := restorer.Restore(ctx, restored); err != nil {
		return err
	}
	a.interactionMu.Lock()
	if a.interactionSessions == nil {
		a.interactionSessions = make(map[string]string)
	}
	for id, owner := range owners {
		a.interactionSessions[id] = owner
	}
	a.interactionMu.Unlock()
	return nil
}

func (a *app) interactionBelongsTo(id, sessionID string) bool {
	if sessionID == "" {
		return true
	}
	a.interactionMu.RLock()
	owner, known := a.interactionSessions[id]
	a.interactionMu.RUnlock()
	return known && owner == sessionID
}

// sensitiveGate returns the registry pre-execution gate for the configured
// sensitive tools (ADR 决策 M6d / dispatch-m6d-2 §4). The registry calls it for
// every whitelisted execution; tools outside sensitive_tools pass through
// untouched. A gated tool first creates a pending approval request, then blocks
// on the CLI serial path reading the user's y/n answer from the terminal (D5),
// then records the decision (Engine.Resolve) and re-reads it through
// Engine.Await (the caller-driven poll — the resolution made here becomes
// visible on the next probe). Approved returns nil and the tool runs; rejected
// appends the interact/deny fact and returns a denial the model sees as a
// tool/error.
func (a *app) sensitiveGate(eng interact.Engine, onEvent func(string, any)) func(context.Context, string, any) error {
	sensitive := a.cfg.Interact.SensitiveTools
	return func(ctx context.Context, name string, args any) error {
		if !containsSensitive(sensitive, name) {
			return nil // not a sensitive tool; no approval needed
		}
		rawArgs, _ := json.Marshal(args)
		argsText := boundRunes(string(rawArgs))
		prompt := fmt.Sprintf("Allow the sensitive tool %s to run? args: %s", name, argsText)
		req, err := eng.Request(ctx, prompt, name, argsText)
		if err != nil {
			return fmt.Errorf("interact: %s approval request failed: %w", name, err)
		}
		onEvent(session.EventInteractRequest, session.NewInteractRequestDetail(req.ID, name, req.Prompt, req.Args, req.Questions))
		approved := false
		if isWebApprovalContext(ctx) {
			// The browser resolves the same engine through /api/interactions;
			// Await keeps the serial tool path blocked until that decision arrives.
			resolved, awaitErr := eng.Await(ctx, req.ID)
			if awaitErr != nil {
				return fmt.Errorf("interact: %s web approval wait failed: %w", name, awaitErr)
			}
			approved = resolved.Status == interact.StatusApproved
		} else {
			var err error
			approved, err = a.approvePrompt(req.ID, prompt)
			if err != nil {
				return fmt.Errorf("interact: %s approval read failed: %w", name, err)
			}
			status := interact.StatusRejected
			if approved {
				status = interact.StatusApproved
			}
			if _, err := eng.Resolve(ctx, req.ID, status); err != nil {
				return fmt.Errorf("interact: %s resolve failed: %w", name, err)
			}
			if _, err := eng.Await(ctx, req.ID); err != nil {
				return fmt.Errorf("interact: %s approval wait failed: %w", name, err)
			}
			onEvent(session.EventInteractResolve, session.NewInteractResolve(req.ID, approved))
		}
		if !approved {
			onEvent(session.EventInteractDeny, session.NewInteractDeny(req.ID))
			return fmt.Errorf("interact: %s denied by user (request %s)", name, req.ID)
		}
		return nil
	}
}

// approvePrompt prints the approval request to the terminal and blocks reading
// the user's y/n answer on the serial path (D5). It reads from a.approveInput
// (os.Stdin by default) so the wiring tests can inject canned answers. Bare
// Enter and EOF count as no (fail-closed for a security gate); y/yes allow,
// n/no deny, anything else re-prompts.
func (a *app) approvePrompt(id, prompt string) (bool, error) {
	fmt.Printf("\n⚠ approval request %s\n%s\n", id, prompt)
	in := a.approveInput
	if in == nil {
		in = os.Stdin
	}
	r := bufio.NewReader(in)
	for {
		fmt.Print("  allow execution? [y/N] > ")
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		case "n", "no", "":
			return false, nil
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
	}
}

// containsSensitive reports whether name is listed in the configured sensitive
// tools (exact match, mirroring the whitelist's own exact-name semantics in
// tools.Policy.Allows).
func containsSensitive(list []string, name string) bool {
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}

// boundRunes trims s to approveArgsBound runes so an over-long tool args
// payload can never make the approval Request fail on the gate path.
func boundRunes(s string) string {
	runes := []rune(s)
	if len(runes) > approveArgsBound {
		return string(runes[:approveArgsBound])
	}
	return s
}
