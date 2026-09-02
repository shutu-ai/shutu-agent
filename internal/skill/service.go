// Package skill defines the skill capability seam (design.md §10 D2, ADR
// 2026-08-18-m5-agent-core.md 决策 ④): a multi-provider skill registry that
// lets consumers discover reusable instructions (name + description) and load
// full bodies on demand. Providers declare what skills exist (List) and load a
// winning candidate's body (Get); the Registry merges provider catalogs,
// resolves same-name conflicts by rank (lower wins, then provider registration
// order, then local order), sorts the winners by name, and rejects a loaded
// definition whose name no longer matches the requested one.
//
// The default provider is the filesystem provider (filesystem.go), which
// discovers <name>/SKILL.md bundles and flat <name>.md files from project and
// user roots, non-recursively. Skills are trusted local files loaded as model
// instruction text — never executed. The catalog injection and skill tool
// wiring are M5d-2 (ADR 决策 ④ 裁剪). This package never imports config, jobs,
// subagent, session or the loop; consumers depend only on the seam's
// interfaces (D2).
package skill

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// nameRe is the skill-name grammar: kebab-case, lowercase ASCII letters and
// digits separated by single hyphens (dispatch-m5d 技能身份).
var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// IsSkillName reports whether name matches the kebab-case skill-name grammar.
func IsSkillName(name string) bool {
	return nameRe.MatchString(name)
}

// Discovery source values (dispatch-m5d 发现优先级; ADR 决策 ④). The value is
// prompt-visible metadata, not precedence by itself — precedence comes from
// the root Rank.
const (
	SourceProjectDSH    = "project-dsh"    // <projectRoot>/.dsh/skills
	SourceProjectAgents = "project-agents" // <projectRoot>/.agents/skills
	SourceCustom        = "custom"         // FSOpts.Dirs
	SourceUserDSH       = "user-dsh"       // <userHome>/.dsh/skills
)

// Candidate is one discovered skill offered by a Provider (ADR 决策 ④). Name is
// the kebab-case identity used to address the skill; Rank orders same-name
// conflicts (lower wins); Path is the absolute source path for filesystem
// providers.
type Candidate struct {
	Name        string
	Description string
	Source      string
	Rank        int
	Path        string
	// Invocation carries the dsh frontmatter policy when the provider can
	// discover it without loading the full body. A nil policy means the
	// provider did not publish policy metadata; dsh defaults both entry
	// points to enabled in that case.
	Invocation *InvocationPolicy
}

// InvocationPolicy controls which actor may load a skill. It is shared by
// candidates and definitions so model-facing consumers can filter user-only
// skills without reading their bodies.
type InvocationPolicy struct {
	ModelInvocable bool
	UserInvocable  bool
}

// CandidateModelInvocable reports the dsh-compatible default for candidates
// from providers that do not expose frontmatter metadata.
func CandidateModelInvocable(c Candidate) bool {
	return c.Invocation == nil || c.Invocation.ModelInvocable
}

// Definition is a fully loaded skill (ADR 决策 ④). Content is the Markdown
// instruction body (frontmatter removed, trimmed). ModelInvocable and
// UserInvocable come from the skill's invocation frontmatter
// (disable-model-invocation / user-invocable); both default to true.
type Definition struct {
	Name        string
	Description string
	Content     string
	Source      string
	Path        string
	// Provider and ResourceBase are the DSH model-facing provenance fields.
	// Providers that predate these fields may leave them empty; the registry
	// fills Provider from the owning provider name.
	Provider       string
	ResourceBase   *ResourceBase
	ModelInvocable bool
	UserInvocable  bool
	// Invocation is non-nil when the provider explicitly supplied invocation
	// metadata. A nil value preserves the dsh default (both enabled) for
	// providers that only implement the original body-loading seam.
	Invocation *InvocationPolicy
}

// ResourceBase describes how a loaded skill resolves relative resources.
// It is the Go representation of DSH's directory/url/opaque union.
type ResourceBase struct {
	Kind        string
	Path        string
	URL         string
	Description string
}

// DefinitionModelInvocable reports the dsh-compatible default for definitions
// from providers that do not publish invocation metadata.
func DefinitionModelInvocable(def *Definition) bool {
	return def != nil && (def.Invocation == nil || def.Invocation.ModelInvocable)
}

// Provider is one skill backend (ADR 决策 ④, D2). Multiple providers coexist in
// a Registry under distinct names. List returns the provider's candidates with
// no same-name resolution — the Registry decides winners; Get loads the full
// body for a candidate this provider returned. Providers must be safe for
// concurrent use.
type Provider interface {
	Name() string
	List(ctx context.Context) ([]Candidate, error)
	Get(ctx context.Context, c Candidate) (*Definition, error)
}

// Registry is the skill Service (ADR 决策 ④): a multi-provider registry that
// merges provider catalogs, resolves same-name conflicts (lower rank wins;
// ties break by provider registration order then local order), sorts winners
// by name, and loads full bodies on demand. Consumers (M5d-2) depend only on
// this interface.
type Registry interface {
	// RegisterProvider adds a provider under its Name; a duplicate name is
	// rejected. Registering after Close is rejected.
	RegisterProvider(p Provider) error
	// List returns the merged, rank-resolved winners sorted by name.
	List(ctx context.Context) ([]Candidate, error)
	// Get loads the full definition for the winning candidate of name. It
	// returns (nil, nil) when no candidate matches. A loaded definition whose
	// Name no longer matches the requested name is rejected with an error.
	Get(ctx context.Context, name string) (*Definition, error)
	// Close marks the registry closed and releases every registered provider
	// that implements io.Closer. After Close, Register/List/Get are rejected.
	Close() error
}

// Sentinel errors returned by the Registry so callers can distinguish
// failures without parsing message text.
var (
	ErrInvalidName       = errors.New("skill: invalid skill name")
	ErrDuplicateProvider = errors.New("skill: provider already registered")
	ErrProviderClosed    = errors.New("skill: registry closed")
	ErrInvalidProvider   = errors.New("skill: invalid provider")
)

// NewRegistry returns an empty skill Registry.
func NewRegistry() Registry {
	return &registry{closeDone: make(chan struct{})}
}

// indexedProvider tracks one registered provider and its registration order.
type indexedProvider struct {
	p     Provider
	order int
}

// indexedCandidate is a provider candidate tagged with the ordering keys that
// break rank ties: provider registration order, then the provider's local
// (list) order.
type indexedCandidate struct {
	candidate     Candidate
	provider      Provider
	providerOrder int
	localOrder    int
}

// winner is a rank-resolved (first-for-name) candidate plus the provider that
// owns it, kept for Get.
type winner struct {
	provider  Provider
	candidate Candidate
}

// registry is the default Registry implementation: an ordered, in-memory
// provider registry guarded by a mutex.
type registry struct {
	mu        sync.Mutex
	providers []*indexedProvider
	nextOrder int
	closed    bool
	closeDone chan struct{}
}

// closer is the optional extension a Provider implements to release its
// resources when the Registry is closed.
type closer interface {
	Close() error
}

func (r *registry) RegisterProvider(p Provider) error {
	if p == nil {
		return fmt.Errorf("%w: nil provider", ErrInvalidProvider)
	}
	name := p.Name()
	if name == "" {
		return fmt.Errorf("%w: provider name must be non-empty", ErrInvalidProvider)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrProviderClosed
	}
	for _, ip := range r.providers {
		if ip.p.Name() == name {
			return fmt.Errorf("%w: %s", ErrDuplicateProvider, name)
		}
	}
	r.providers = append(r.providers, &indexedProvider{p: p, order: r.nextOrder})
	r.nextOrder++
	return nil
}

func (r *registry) List(ctx context.Context) ([]Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	providers := r.snapshotProviders()
	if providers == nil {
		return nil, ErrProviderClosed
	}
	winners, err := r.collect(ctx, providers)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(winners))
	for _, w := range winners {
		out = append(out, w.candidate)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *registry) Get(ctx context.Context, name string) (*Definition, error) {
	if !IsSkillName(name) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	providers := r.snapshotProviders()
	if providers == nil {
		return nil, ErrProviderClosed
	}
	winners, err := r.collect(ctx, providers)
	if err != nil {
		return nil, err
	}
	for _, w := range winners {
		if w.candidate.Name != name {
			continue
		}
		def, err := w.provider.Get(ctx, w.candidate)
		if err != nil {
			return nil, fmt.Errorf("skill: load %q: %w", name, err)
		}
		if def == nil {
			return nil, nil
		}
		if def.Name != name {
			return nil, fmt.Errorf("skill: provider %q returned definition %q for skill %q", w.provider.Name(), def.Name, name)
		}
		if def.Provider == "" {
			def.Provider = w.provider.Name()
		}
		return def, nil
	}
	return nil, nil
}

func (r *registry) Close() error {
	r.mu.Lock()
	if r.closed {
		done := r.closeDone
		r.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	r.closed = true
	ps := make([]*indexedProvider, len(r.providers))
	copy(ps, r.providers)
	r.mu.Unlock()

	var first error
	for _, ip := range ps {
		if c, ok := ip.p.(closer); ok {
			if err := c.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	close(r.closeDone)
	return first
}

// snapshotProviders returns a copy of the provider list under lock, or nil
// when the registry is closed.
func (r *registry) snapshotProviders() []*indexedProvider {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	ps := make([]*indexedProvider, len(r.providers))
	copy(ps, r.providers)
	return ps
}

// collect merges every provider's catalog into rank-resolved winners: sort all
// candidates by (rank asc, provider registration order asc, local order asc),
// then keep the first candidate for each name. A provider failure or an
// invalid candidate is a hard error (fail-closed; a personal agent's skill
// roots are static and the filesystem provider treats missing roots as empty,
// so day-to-day discovery never errors).
func (r *registry) collect(ctx context.Context, providers []*indexedProvider) ([]winner, error) {
	var all []indexedCandidate
	for _, ip := range providers {
		cands, err := ip.p.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("skill: provider %q: %w", ip.p.Name(), err)
		}
		for i, c := range cands {
			if err := validateCandidate(c); err != nil {
				return nil, fmt.Errorf("skill: provider %q: %w", ip.p.Name(), err)
			}
			all = append(all, indexedCandidate{candidate: c, provider: ip.p, providerOrder: ip.order, localOrder: i})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.candidate.Rank != b.candidate.Rank {
			return a.candidate.Rank < b.candidate.Rank
		}
		if a.providerOrder != b.providerOrder {
			return a.providerOrder < b.providerOrder
		}
		return a.localOrder < b.localOrder
	})
	out := make([]winner, 0, len(all))
	seen := make(map[string]struct{}, len(all))
	for _, ic := range all {
		if _, dup := seen[ic.candidate.Name]; dup {
			continue
		}
		seen[ic.candidate.Name] = struct{}{}
		out = append(out, winner{provider: ic.provider, candidate: ic.candidate})
	}
	return out, nil
}

// validateCandidate rejects a provider candidate that cannot be addressed by
// name or shown in a catalog.
func validateCandidate(c Candidate) error {
	if !IsSkillName(c.Name) {
		return fmt.Errorf("%w: %q", ErrInvalidName, c.Name)
	}
	if strings.TrimSpace(c.Description) == "" {
		return fmt.Errorf("skill: candidate %q has an empty description", c.Name)
	}
	return nil
}
