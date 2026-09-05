// Package crashboundary is the machine-readable authority for what happens to
// an externally visible operation when the host process dies. Transport code
// and release gates must consult these contracts instead of inventing recovery
// semantics from event names.
package crashboundary

import (
	"errors"
	"fmt"
	"sort"
)

type CrashPolicy string

const (
	// AtMostOnce means a host crash may lose the operation after the external
	// peer has observed it. It must never be replayed automatically.
	AtMostOnce CrashPolicy = "at-most-once"
	// RetryableReceipt means a durable owner receipt defines recovery and
	// repeated recovery remains idempotent.
	RetryableReceipt CrashPolicy = "retryable-receipt"
	// AuditedUnorderedFailure means an operation or its failure is audited, but
	// ordering and automatic retry are not promised.
	AuditedUnorderedFailure CrashPolicy = "audited-unordered-failure"
	// RuntimeState is configuration/process ownership metadata, not a durable
	// domain receipt for an external effect.
	RuntimeState CrashPolicy = "runtime-state"
)

type TransportFailurePolicy string

const (
	// NoAutomaticReplay is mandatory for failed MCP/network transports.
	NoAutomaticReplay TransportFailurePolicy = "no-automatic-replay"
	// DurableReceiptRecovery means only a committed receipt can be replayed.
	DurableReceiptRecovery TransportFailurePolicy = "durable-receipt-recovery"
	// AuditedFailure is retained for diagnosis without promising ordering/retry.
	AuditedFailure TransportFailurePolicy = "audited-failure"
)

type LifecycleClassification string

const (
	ExternalOperation LifecycleClassification = "external-operation"
	RuntimeStateOnly  LifecycleClassification = "runtime-state"
)

var (
	ErrUnknownContract   = errors.New("crash boundary is unknown")
	ErrNoAutomaticReplay = errors.New("failed transport must not replay automatically")
)

type Contract struct {
	ID                      string                  `json:"id"`
	Family                  string                  `json:"family"`
	ExternalEffect          string                  `json:"externalEffect"`
	CrashPolicy             CrashPolicy             `json:"crashPolicy"`
	TransportFailurePolicy  TransportFailurePolicy  `json:"transportFailurePolicy"`
	LifecycleClassification LifecycleClassification `json:"lifecycleClassification"`
	ReceiptEvent            string                  `json:"receiptEvent,omitempty"`
	Recovery                string                  `json:"recovery,omitempty"`
	ProcessDeathLosesEffect bool                    `json:"processDeathLosesEffect"`
	AutomaticReplay         bool                    `json:"automaticReplay"`
	Evidence                []string                `json:"evidence"`
}

type Registry struct {
	contracts map[string]Contract
}

func Required() *Registry {
	contracts := []Contract{
		{
			ID: "mcp.call", Family: "mcp",
			ExternalEffect:          "remote MCP tools/call",
			CrashPolicy:             AtMostOnce,
			TransportFailurePolicy:  NoAutomaticReplay,
			LifecycleClassification: ExternalOperation,
			ReceiptEvent:            "mcp/call (audit only)",
			Recovery:                "failed/interrupted transports stay terminal; one later explicit caller may retry",
			ProcessDeathLosesEffect: true,
			Evidence:                []string{"internal/mcp/reconnect_test.go", "internal/mcp/http_test.go", "cmd/sta/mcps_test.go"},
		},
		{
			ID: "terminal.foreground.write", Family: "terminal",
			ExternalEffect:          "bytes delivered to a persistent terminal process",
			CrashPolicy:             AtMostOnce,
			TransportFailurePolicy:  NoAutomaticReplay,
			LifecycleClassification: ExternalOperation,
			Recovery:                "close the owned process tree and report an unknown foreground outcome",
			ProcessDeathLosesEffect: true,
			Evidence:                []string{"cmd/sta/terminal_test.go", "internal/terminal/terminal_test.go"},
		},
		{
			ID: "terminal.lifecycle", Family: "terminal",
			ExternalEffect:          "terminal child process ownership",
			CrashPolicy:             AtMostOnce,
			TransportFailurePolicy:  NoAutomaticReplay,
			LifecycleClassification: RuntimeStateOnly,
			ReceiptEvent:            "terminal/start and terminal/stop (runtime audit)",
			Recovery:                "cold restart records one stale stop receipt; it never resurrects a process",
			ProcessDeathLosesEffect: true,
			Evidence:                []string{"cmd/sta/terminal_test.go"},
		},
		{
			ID: "schedule.fire", Family: "schedule",
			ExternalEffect:          "owner wake for a due reminder",
			CrashPolicy:             RetryableReceipt,
			TransportFailurePolicy:  DurableReceiptRecovery,
			LifecycleClassification: ExternalOperation,
			ReceiptEvent:            "schedule/fire plus agent/inbox/spliced",
			Recovery:                "crash windows replay to one receipt and one fire fact",
			AutomaticReplay:         true,
			Evidence:                []string{"cmd/sta/schedules_test.go"},
		},
		{
			ID: "workflow.child", Family: "workflow",
			ExternalEffect:          "workflow child agent/process invocation",
			CrashPolicy:             AtMostOnce,
			TransportFailurePolicy:  NoAutomaticReplay,
			LifecycleClassification: ExternalOperation,
			ReceiptEvent:            "workflow/agent-start (audit only)",
			Recovery:                "a crash-open run is audited and never re-invokes an already-started child",
			ProcessDeathLosesEffect: true,
			Evidence:                []string{"internal/workflow/tools_test.go", "internal/workflow/node/node_test.go"},
		},
		{
			ID: "subagent.publication", Family: "subagent",
			ExternalEffect:          "child Agent publication and later completion wake",
			CrashPolicy:             RetryableReceipt,
			TransportFailurePolicy:  DurableReceiptRecovery,
			LifecycleClassification: ExternalOperation,
			ReceiptEvent:            "subagent/start plus agent/inbox/spliced",
			Recovery:                "cold recovery materializes one owner receipt from the durable terminal fact",
			AutomaticReplay:         true,
			Evidence:                []string{"internal/subagent/tools_test.go", "cmd/sta/agent_runtime_recovery_test.go"},
		},
		{
			ID: "plugin.call", Family: "plugin",
			ExternalEffect:          "plugin-owned tool execution",
			CrashPolicy:             AtMostOnce,
			TransportFailurePolicy:  NoAutomaticReplay,
			LifecycleClassification: ExternalOperation,
			ReceiptEvent:            "tool/call and tool/result (audit only)",
			Recovery:                "the interrupted tool result records outcome unknown; callers must not replay blindly",
			ProcessDeathLosesEffect: true,
			Evidence:                []string{"internal/plugin/registry_test.go", "internal/tools/tools_test.go"},
		},
		{
			ID: "plugin.generation.reload", Family: "plugin",
			ExternalEffect:          "plugin generation registration and old-generation disposal",
			CrashPolicy:             AuditedUnorderedFailure,
			TransportFailurePolicy:  AuditedFailure,
			LifecycleClassification: RuntimeStateOnly,
			ReceiptEvent:            "plugin inventory/generation facts",
			Recovery:                "reload is explicit; a failed generation is retired without silent tool replay",
			ProcessDeathLosesEffect: true,
			Evidence:                []string{"internal/plugin/registry_test.go"},
		},
	}
	registry := &Registry{contracts: make(map[string]Contract, len(contracts))}
	for _, contract := range contracts {
		if err := contract.validate(); err != nil {
			panic(err)
		}
		registry.contracts[contract.ID] = contract
	}
	return registry
}

func (c Contract) validate() error {
	if c.ID == "" || c.Family == "" || c.ExternalEffect == "" || len(c.Evidence) == 0 {
		return fmt.Errorf("crash contract %q is missing identity/effect evidence", c.ID)
	}
	switch c.CrashPolicy {
	case AtMostOnce:
		if !c.ProcessDeathLosesEffect || c.AutomaticReplay {
			return fmt.Errorf("crash contract %q has inconsistent at-most-once flags", c.ID)
		}
	case RetryableReceipt:
		if c.ProcessDeathLosesEffect || !c.AutomaticReplay || c.ReceiptEvent == "" || c.Recovery == "" {
			return fmt.Errorf("crash contract %q has inconsistent retryable-receipt flags", c.ID)
		}
	case AuditedUnorderedFailure:
		if !c.ProcessDeathLosesEffect || c.AutomaticReplay {
			return fmt.Errorf("crash contract %q has inconsistent audited-failure flags", c.ID)
		}
	case RuntimeState:
		if c.ReceiptEvent == "" || c.Recovery == "" {
			return fmt.Errorf("crash contract %q runtime-state requires lifecycle audit metadata", c.ID)
		}
	default:
		return fmt.Errorf("crash contract %q has invalid crash policy %q", c.ID, c.CrashPolicy)
	}
	switch c.TransportFailurePolicy {
	case NoAutomaticReplay:
		if c.AutomaticReplay {
			return fmt.Errorf("crash contract %q allows replay after a no-replay transport failure", c.ID)
		}
	case DurableReceiptRecovery, AuditedFailure:
	default:
		return fmt.Errorf("crash contract %q has invalid transport policy %q", c.ID, c.TransportFailurePolicy)
	}
	switch c.LifecycleClassification {
	case ExternalOperation, RuntimeStateOnly:
	default:
		return fmt.Errorf("crash contract %q has invalid lifecycle classification", c.ID)
	}
	return nil
}

func (r *Registry) Get(id string) (Contract, error) {
	if r == nil {
		return Contract{}, ErrUnknownContract
	}
	contract, ok := r.contracts[id]
	if !ok {
		return Contract{}, fmt.Errorf("%w: %q", ErrUnknownContract, id)
	}
	return contract, nil
}

func (r *Registry) List() []Contract {
	if r == nil {
		return nil
	}
	out := make([]Contract, 0, len(r.contracts))
	for _, contract := range r.contracts {
		out = append(out, contract)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
