package profile

import (
	"fmt"
	"sort"

	"github.com/jabing/shutu-agent/internal/code"
)

// Classification is the release-composition class assigned by the pinned
// DSH bundle patches. Required means at least one claimed DSH profile mounts
// the capability; optional means an explicit opt-in provider or backend.
type Classification string

const (
	ClassificationRequired Classification = "required"
	ClassificationOptional Classification = "optional"
)

// Capability is one authoritative row from the reference capability-seam
// inventory. It records the reference composition, the Go subject state, and
// the enforcing implementation or stable unsupported reason.
type Capability struct {
	ID             string         `json:"id"`
	Classification Classification `json:"classification"`
	Profiles       []string       `json:"profiles,omitempty"`
	State          State          `json:"state"`
	Enforcement    string         `json:"enforcement,omitempty"`
	Implementation string         `json:"implementation,omitempty"`
	Replay         string         `json:"replay,omitempty"`
	Reason         string         `json:"reason,omitempty"`
}

const (
	ProfileDSHBase  = "dsh-base"
	ProfileWeb      = "web"
	ProfileHeadless = "headless"
)

// CapabilityIDs is deliberately fixed. A reference seam added or removed is a
// baseline change and must update this release authority explicitly.
var CapabilityIDs = []string{
	"ctx.attachments", "ctx.llm", "ctx.tokenMeter", "ctx.toolResultPruner",
	"ctx.sessions", "ctx.invariants", "ctx.typert", "ctx.typertGateway",
	"ctx.sessionPersistence", "ctx.settings", "ctx.credentials",
	"ctx.sessionTelemetry", "ctx.storage", "ctx.storageDomain",
	"ctx.messageFeedback", "ctx.workspaceRegistry", "ctx.sessionQuery",
	"ctx.fileReferences", "ctx.sessionReferenceResolver", "ctx.sessionTitle",
	"ctx.systemPrompt", "ctx.tools", "ctx.userQuestions", "ctx.planMode",
	"ctx.agentPresets", "ctx.commands", "ctx.sessionProjections",
	"ctx.sessionProjectionCache", "ctx.skills", "ctx.agents",
	"ctx.agentDefaultModel", "ctx.agentLoop", "ctx.goals", "ctx.e2b",
	"ctx.subprocess", "ctx.shell", "ctx.shellEnv", "ctx.terminals",
	"ctx.sandbox", "ctx.sandboxPolicy", "ctx.approval",
	"ctx.permissionPresets", "ctx.codeRuntime", "ctx.fs", "ctx.compaction",
	"ctx.subagents", "ctx.agentTeams", "ctx.jobs", "ctx.web",
	"ctx.spillStore", "ctx.directoryPicker", "ctx.webServer",
	"ctx.clientModules", "ctx.workflowEngine", "ctx.lsp", "ctx.apiProxy",
	"ctx.dynamicCordisRunner", "ctx.cordisInspect",
}

func required(id, enforcement, implementation, replay string) Capability {
	return Capability{
		ID: id, Classification: ClassificationRequired,
		Profiles: []string{ProfileDSHBase}, State: StateAvailable,
		Enforcement: enforcement, Implementation: implementation, Replay: replay,
	}
}

func requiredWeb(id, enforcement, implementation, replay string) Capability {
	capability := required(id, enforcement, implementation, replay)
	capability.Profiles = []string{ProfileWeb}
	return capability
}

func unsupported(id string, classification Classification, profiles []string, reason string) Capability {
	return Capability{
		ID: id, Classification: classification, Profiles: profiles,
		State: StateUnsupported, Reason: reason,
	}
}

// Classifications returns the fixed mapping of every capability seam in the
// pinned reference's docs/capability-seams.md to the Go release state.
func Classifications() []Capability {
	codeRuntimeAvailable, codeRuntimeReason := code.TypeScriptRuntimeStatus()
	codeRuntime := required("ctx.codeRuntime", "external Node permission-model process with wall/compute CPU, heap/output quotas, owned process-group/job cleanup, and permission-denied ambient child/worker effects; architecture substitute for the DSH worker-thread transport", "internal/code", "cross-platform worker timeout/cleanup/permission matrix")
	if !codeRuntimeAvailable {
		codeRuntime = unsupported("ctx.codeRuntime", ClassificationRequired, []string{ProfileDSHBase}, codeRuntimeReason)
	}
	sandboxAvailable, sandboxReason := code.LocalSandboxStatus()
	sandbox := required("ctx.sandbox", code.LocalSandboxDiagnostic().Summary+" Fail-closed rejection is required when no backend is available.", "internal/tools; internal/code", "sandbox and unsupported-platform negative tests")
	if !sandboxAvailable {
		sandbox = unsupported("ctx.sandbox", ClassificationRequired, []string{ProfileDSHBase}, sandboxReason)
	}
	capabilities := []Capability{
		required("ctx.attachments", "authorized attachment IDs with bounded image admission", "internal/attachment", "attachment lifecycle and rich-content tests"),
		required("ctx.llm", "provider-neutral stream seam with pinned route admission", "internal/llm", "provider protocol and capability tests"),
		required("ctx.tokenMeter", "session-isolated usage folds with durable replay", "internal/meter", "meter replay and completion-ledger tests"),
		required("ctx.toolResultPruner", "bounded model-visible result replacement", "internal/compaction; internal/tools", "surface replacement and output-bound tests"),
		required("ctx.sessions", "append-only canonical event log and derived history", "internal/session", "event/history/recovery contract tests"),
		required("ctx.invariants", "owner-local structural checks before publication", "internal/session; internal/contractfixture", "wire and lifecycle contract tests"),
		required("ctx.typert", "bounded RPC/tool descriptors with generated schemas", "internal/tools; internal/webserver", "catalog and native wire contract tests"),
		required("ctx.typertGateway", "native transport dispatch with method identity checks", "internal/webserver", "native RPC request/response tests"),
		required("ctx.sessionPersistence", "SQLite/JSONL append-only durability with repair", "internal/store; internal/persistence", "persistence corruption and lifecycle suites"),
		required("ctx.settings", "durable settings with composition defaults", "internal/config; internal/store", "settings load/store/restart tests"),
		required("ctx.credentials", "dedicated value boundary with write-only Web projection", "internal/credential; cmd/pa", "credential rotation/rollback/redaction tests"),
		required("ctx.sessionTelemetry", "best-effort bounded OTLP export that cannot own durable state", "internal/observability", "telemetry shutdown/identity/redaction tests"),
		requiredWeb("ctx.storage", "typed durable state through SQLite and attachment-backed stores", "internal/store; internal/attachment", "workspace, feedback and attachment tests"),
		requiredWeb("ctx.storageDomain", "one lifecycle-bound domain facade over durable typed state", "cmd/pa; internal/store", "composition cold-restart tests"),
		requiredWeb("ctx.messageFeedback", "CAS feedback with stable message identity", "internal/store; internal/webserver", "feedback API and native identity tests"),
		requiredWeb("ctx.workspaceRegistry", "workspace identity, grouping and ordering boundary", "internal/store; cmd/pa; internal/webserver", "workspace lifecycle/order tests"),
		required("ctx.sessionQuery", "bounded exact reads and authorized search", "internal/sessionquery; internal/webserver", "session-query authorization/replay tests"),
		requiredWeb("ctx.fileReferences", "cwd-bounded path-only candidate discovery", "cmd/pa; internal/webserver", "file-reference wire tests"),
		requiredWeb("ctx.sessionReferenceResolver", "bounded durable conversation snapshots for mentions", "internal/sessionquery; internal/webserver", "session reference candidate tests"),
		required("ctx.sessionTitle", "deterministic fallback plus bounded async model title", "cmd/pa/title.go", "title fallback/restart tests"),
		required("ctx.systemPrompt", "one prompt/tool projection assembled per runtime", "internal/prompt; cmd/pa", "prompt/catalog snapshot tests"),
		required("ctx.tools", "policy-gated registry with canonical rich results", "internal/tools", "tool execution/rich result/error tests"),
		required("ctx.userQuestions", "provider-neutral human question lifecycle", "internal/interact", "question timeout/replay tests"),
		required("ctx.planMode", "durable plan mode with approved exit boundary", "internal/plan; cmd/pa", "plan mode runtime tests"),
		requiredWeb("ctx.agentPresets", "preset authoring and scoped Agent composition", "cmd/pa/native_agent_presets.go", "preset list/select/authoring tests"),
		required("ctx.commands", "human command registry separate from model tools", "cmd/pa/web_command.go", "command lifecycle/catalog tests"),
		required("ctx.sessionProjections", "event-fold projection authority", "internal/webserver/native_projection.go", "native projection replay tests"),
		requiredWeb("ctx.sessionProjectionCache", "revisioned projection checkpoint and cold-read ladder", "internal/projection; internal/store; internal/webserver", "projection cache/revision/cold-read tests"),
		required("ctx.skills", "merged filesystem skill catalog and scoped invocation", "internal/skill; cmd/pa", "skill discovery/invocation tests"),
		required("ctx.agents", "owned live Agent handles and lineage registry", "internal/agent; cmd/pa/agent_runtime.go", "Agent publication/disposal tests"),
		required("ctx.agentDefaultModel", "shared persisted default model route", "cmd/pa/modelcatalog.go", "model selection/catalog tests"),
		required("ctx.agentLoop", "turn/step waterfall with cancellation and inbox wakeups", "internal/loop; internal/agent", "waterfall/recovery tests"),
		required("ctx.goals", "revisioned durable goal rounds and activation", "internal/goal", "goal continuation/round tests"),
		unsupported("ctx.e2b", ClassificationOptional, []string{"optional:e2b"}, "optional E2B remote sandbox backend is not composed"),
		required("ctx.subprocess", "owned process trees, stdio, cancellation and teardown", "internal/tools; internal/terminal; internal/code", "process ownership/cancellation tests"),
		required("ctx.shell", "platform shell route with bounded output", "internal/tools", "shell timeout/cancellation/redaction tests"),
		required("ctx.shellEnv", "credential-scrubbed effect-scoped environment", "internal/tools/env.go", "hostile environment tests"),
		unsupported("ctx.terminals", ClassificationOptional, []string{"optional:terminal"}, "persistent terminal backend is intentionally not selected in the local release profile"),
		sandbox,
		required("ctx.sandboxPolicy", "one shared read-only/workspace/full policy source", "internal/tools/policy.go; internal/config", "policy projection/config tests"),
		required("ctx.approval", "closed-outcome approval requests with durable receipts", "internal/interact; internal/session", "approval timeout/replay/CAS tests"),
		required("ctx.permissionPresets", "preset changes update sandbox and approval together", "internal/config; cmd/pa", "permission runtime tests"),
		codeRuntime,
		required("ctx.fs", "workspace-bounded reads, writes, edits and observation", "internal/fs", "filesystem contract/negative tests"),
		required("ctx.compaction", "pressure-driven replayable surface replacement", "internal/compaction", "compaction recovery tests"),
		required("ctx.subagents", "parent-owned child lifecycle and lineage authorization", "internal/subagent; internal/team", "subagent ownership/recovery tests"),
		unsupported("ctx.agentTeams", ClassificationOptional, []string{"optional:agent-teams"}, "experimental Team profile is not selected in this release composition"),
		required("ctx.jobs", "owner-scoped background jobs with durable receipts", "internal/jobs", "job lifecycle/recovery tests"),
		required("ctx.web", "provider-neutral search/fetch with bounded cancellation", "internal/web", "search/fetch fault and cancellation tests"),
		required("ctx.spillStore", "bounded inline output with retrievable spill locator", "internal/tools/spill.go", "spill failure/redaction tests"),
		requiredWeb("ctx.directoryPicker", "OS picker with directory browse/create fallback", "internal/webserver/native_rpc.go", "host directory browse/create tests"),
		requiredWeb("ctx.webServer", "authenticated HTTP and WebSocket carrier", "internal/webserver", "auth/replay/shutdown matrix"),
		unsupported("ctx.clientModules", ClassificationOptional, []string{"optional:client-modules"}, "architecture-specific DSH browser module graph is outside the selected Go web profile"),
		required("ctx.workflowEngine", "owned workflow worker with terminal receipts", "internal/workflow", "worker death/late-output tests"),
		unsupported("ctx.lsp", ClassificationOptional, []string{"optional:lsp"}, "LSP backend is not selected in this release composition"),
		requiredWeb("ctx.apiProxy", "native client-request/response and stream gateway", "internal/webserver/native_rpc.go", "native RPC/mux contract tests"),
		unsupported("ctx.dynamicCordisRunner", ClassificationOptional, []string{"optional:cordis-dynamic-runner"}, "architecture-specific Cordis host runner is outside the selected Go runtime profile"),
		unsupported("ctx.cordisInspect", ClassificationOptional, []string{"optional:cordis-inspect"}, "architecture-specific Cordis inspect provider is outside the selected Go runtime profile"),
	}
	byID := make(map[string]Capability, len(capabilities))
	out := make([]Capability, 0, len(capabilities))
	for _, capability := range capabilities {
		if _, exists := byID[capability.ID]; exists {
			panic(fmt.Errorf("profile: duplicate capability %q", capability.ID))
		}
		if err := capability.validate(); err != nil {
			panic(err)
		}
		byID[capability.ID] = capability
		out = append(out, capability)
	}
	for _, id := range CapabilityIDs {
		if _, exists := byID[id]; !exists {
			panic(fmt.Errorf("profile: capability inventory is missing %q", id))
		}
	}
	if len(out) != len(CapabilityIDs) {
		panic(fmt.Errorf("profile: capability inventory has %d rows, want %d", len(out), len(CapabilityIDs)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func GetCapability(id string) (Capability, error) {
	for _, capability := range Classifications() {
		if capability.ID == id {
			return capability, nil
		}
	}
	return Capability{}, fmt.Errorf("%w: %q", ErrUnknownProfile, id)
}

func (c Capability) validate() error {
	switch c.Classification {
	case ClassificationRequired, ClassificationOptional:
	default:
		return fmt.Errorf("capability %q has invalid classification %q", c.ID, c.Classification)
	}
	switch c.State {
	case StateAvailable:
		if c.ID == "" || len(c.Profiles) == 0 || c.Enforcement == "" ||
			c.Implementation == "" || c.Replay == "" || c.Reason != "" {
			return fmt.Errorf("capability %q has an invalid available descriptor", c.ID)
		}
	case StateUnsupported:
		if c.ID == "" || len(c.Profiles) == 0 || c.Enforcement != "" ||
			c.Implementation != "" || c.Replay != "" || c.Reason == "" {
			return fmt.Errorf("capability %q has an invalid unsupported descriptor", c.ID)
		}
	default:
		return fmt.Errorf("capability %q has invalid state %q", c.ID, c.State)
	}
	return nil
}
