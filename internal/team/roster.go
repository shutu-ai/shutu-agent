package team

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/shutu-ai/shutu-agent/internal/agent"
)

// MemberPhase is the durable provisioning lifecycle. Names remain reserved
// after failure so a recovered Team cannot accidentally address a different
// Agent under an old identity.
type MemberPhase string

const (
	MemberProvisioning MemberPhase = "provisioning"
	MemberActive       MemberPhase = "active"
	MemberFailed       MemberPhase = "failed"
)

// MemberSnapshot is the detached roster record persisted in the lead session.
// The live Agent handle is deliberately not serialized; recovery must bind a
// fresh handle to the same immutable member identity before marking it active.
type MemberSnapshot struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Provider    string      `json:"provider,omitempty"`
	Context     string      `json:"context"` // fresh | fork
	Phase       MemberPhase `json:"phase"`
	Error       string      `json:"error,omitempty"`
}

// MemberView adds the current Agent lifecycle state to a durable roster row.
type MemberView struct {
	MemberSnapshot
	Role   string       `json:"role"`   // lead | teammate
	Status agent.Status `json:"status"` // running | idle | closed
}

var (
	ErrMemberExists   = errors.New("team: member name already exists")
	ErrMemberNotFound = errors.New("team: member not found")
	ErrNotLead        = errors.New("team: only the lead may provision teammates")
)

const DefaultMaxMembers = 8

var memberNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Roster binds one Team to the process-wide Agent Registry. It is intentionally
// independent from Board so task/mailbox persistence can be restored before
// live runtimes are reprovisioned.
type Roster struct {
	teamID     string
	leadID     agent.ID
	reg        *agent.Registry
	maxMembers int
	reserveID  func(namespace, id string) (bool, error)

	mu          sync.Mutex
	members     map[string]MemberSnapshot
	handles     map[string]*agent.Handle
	reservedIDs map[string]struct{}
}

// NewRoster creates a roster with one immutable lead identity.
func NewRoster(teamID string, leadID agent.ID, reg *agent.Registry) (*Roster, error) {
	return NewRosterWithLimit(teamID, leadID, reg, DefaultMaxMembers)
}

// NewRosterWithLimit creates a roster with an explicit immutable teammate
// limit. The limit counts failed/provisioning identities too, matching the
// reference contract's never-reuse rule.
func NewRosterWithLimit(teamID string, leadID agent.ID, reg *agent.Registry, maxMembers int) (*Roster, error) {
	if strings.TrimSpace(teamID) == "" || leadID == "" || reg == nil {
		return nil, errors.New("team: team id, lead id and agent registry are required")
	}
	if maxMembers < 1 {
		return nil, errors.New("team: max members must be positive")
	}
	return &Roster{
		teamID:     teamID,
		leadID:     leadID,
		reg:        reg,
		maxMembers: maxMembers,
		members: map[string]MemberSnapshot{
			string(leadID): {ID: string(leadID), Name: "lead", Context: "fresh", Phase: MemberActive},
		},
		handles:     map[string]*agent.Handle{},
		reservedIDs: map[string]struct{}{},
	}, nil
}

// SetIDReservation installs the cross-process identity allocator used by this
// roster. It is optional for in-memory library callers; the application wires
// it to the same durable store used by the Team board.
func (r *Roster) SetIDReservation(reserve func(namespace, id string) (bool, error)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.reserveID = reserve
	r.mu.Unlock()
}

// ReserveMemberID claims the immutable member identity before a caller starts
// the durable member/session provisioning transaction. Spawn consumes this
// reservation without claiming it a second time.
func (r *Roster) ReserveMemberID(name string) (agent.ID, error) {
	if r == nil {
		return "", ErrMemberNotFound
	}
	name = strings.TrimSpace(name)
	if !memberNamePattern.MatchString(name) || len(name) > 64 || name == "lead" {
		return "", errors.New("team: member name must be lower-kebab-case, at most 64 characters, and not lead")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reserveMemberIDLocked(name)
}

// MemberID validates and returns the deterministic compatibility identity
// without claiming a durable reservation. Production callers pair it with the
// atomic TeamMemberSessionReservationStore transaction; legacy callers should
// use ReserveMemberID instead.
func (r *Roster) MemberID(name string) (agent.ID, error) {
	if r == nil {
		return "", ErrMemberNotFound
	}
	name = strings.TrimSpace(name)
	if !memberNamePattern.MatchString(name) || len(name) > 64 || name == "lead" {
		return "", errors.New("team: member name must be lower-kebab-case, at most 64 characters, and not lead")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.members)-1 >= r.maxMembers {
		return "", fmt.Errorf("team: member limit %d reached", r.maxMembers)
	}
	for _, member := range r.members {
		if member.Name == name {
			return "", fmt.Errorf("%w: %s", ErrMemberExists, name)
		}
	}
	id := agent.ID(fmt.Sprintf("%s:%s", r.teamID, name))
	if _, reserved := r.reservedIDs[string(id)]; reserved {
		return "", fmt.Errorf("%w: %s", ErrMemberExists, name)
	}
	return id, nil
}

// AdoptReservedMemberID records an identity already claimed and committed by
// an atomic storage transaction. It is intentionally separate from Reserve so
// the in-memory Roster never attempts a second durable claim.
func (r *Roster) AdoptReservedMemberID(id agent.ID) error {
	if r == nil {
		return ErrMemberNotFound
	}
	id = agent.ID(strings.TrimSpace(string(id)))
	if id == "" {
		return errors.New("team: member id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.members[string(id)]; exists {
		return fmt.Errorf("%w: %s", ErrMemberExists, id)
	}
	r.reservedIDs[string(id)] = struct{}{}
	return nil
}

func (r *Roster) reserveMemberIDLocked(name string) (agent.ID, error) {
	if len(r.members)-1 >= r.maxMembers {
		return "", fmt.Errorf("team: member limit %d reached", r.maxMembers)
	}
	id := agent.ID(fmt.Sprintf("%s:%s", r.teamID, name))
	if _, exists := r.members[string(id)]; exists {
		return "", fmt.Errorf("%w: %s", ErrMemberExists, name)
	}
	if _, reserved := r.reservedIDs[string(id)]; reserved {
		return "", fmt.Errorf("%w: %s", ErrMemberExists, name)
	}
	if r.reserveID != nil {
		claimed, err := r.reserveID("team-member:"+r.teamID, string(id))
		if err != nil {
			return "", fmt.Errorf("team: reserve member id: %w", err)
		}
		if !claimed {
			return "", fmt.Errorf("%w: %s", ErrMemberExists, name)
		}
	}
	r.reservedIDs[string(id)] = struct{}{}
	return id, nil
}

// reserveMemberIDForBoard lets the Board move the reservation before its
// caller publishes session and member records. The roster remains the owner
// of its in-memory duplicate/name state.
func (r *Roster) reserveMemberIDForBoard(name string, reserve func(namespace, id string) (bool, error)) (agent.ID, error) {
	if r == nil {
		return "", ErrMemberNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reserveID == nil {
		r.reserveID = reserve
	}
	return r.reserveMemberIDLocked(name)
}

// LeadID returns the only identity allowed to create teammates.
func (r *Roster) LeadID() agent.ID { return r.leadID }

// IsLead reports whether id is the immutable Team Lead identity.
func (r *Roster) IsLead(id agent.ID) bool { return r != nil && id == r.leadID }

// Spawn provisions a direct child in the same Agent Registry. The member row
// is visible as provisioning before Start; any failure leaves a durable failed
// row and never releases the name for reuse.
func (r *Roster) Spawn(ctx context.Context, actor agent.ID, name, description, provider, contextKind string, runner agent.Runner) (MemberView, error) {
	if r == nil {
		return MemberView{}, ErrMemberNotFound
	}
	if actor != r.leadID {
		return MemberView{}, ErrNotLead
	}
	name = strings.TrimSpace(name)
	if !memberNamePattern.MatchString(name) || len(name) > 64 || name == "lead" || runner == nil {
		return MemberView{}, errors.New("team: member name must be lower-kebab-case, at most 64 characters, and runner is required")
	}
	if contextKind == "" {
		contextKind = "fresh"
	}
	if contextKind != "fresh" && contextKind != "fork" {
		return MemberView{}, errors.New("team: member context must be fresh or fork")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	if len(r.members)-1 >= r.maxMembers {
		r.mu.Unlock()
		return MemberView{}, fmt.Errorf("team: member limit %d reached", r.maxMembers)
	}
	for _, member := range r.members {
		if member.Name == name {
			r.mu.Unlock()
			return MemberView{}, fmt.Errorf("%w: %s", ErrMemberExists, name)
		}
	}
	// Reserve the immutable identity before touching the registry. An app-level
	// provisioning transaction may already have claimed it; direct Roster
	// callers claim it here so every entry point has the same cross-process
	// uniqueness boundary.
	id := agent.ID(fmt.Sprintf("%s:%s", r.teamID, name))
	if _, exists := r.members[string(id)]; exists {
		r.mu.Unlock()
		return MemberView{}, fmt.Errorf("%w: %s", ErrMemberExists, name)
	}
	if _, reserved := r.reservedIDs[string(id)]; !reserved {
		var reserveErr error
		id, reserveErr = r.reserveMemberIDLocked(name)
		if reserveErr != nil {
			r.mu.Unlock()
			return MemberView{}, reserveErr
		}
	}
	row := MemberSnapshot{ID: string(id), Name: name, Description: description, Provider: provider, Context: contextKind, Phase: MemberProvisioning}
	r.members[row.ID] = row
	r.mu.Unlock()

	handle, err := r.reg.Create(agent.Options{ID: id, ParentID: r.leadID, Runner: runner})
	if err == nil {
		err = handle.Start(ctx)
		if err != nil {
			// Registry publication precedes Start by design. A failed start must
			// not leak a published handle even though the roster keeps the name
			// reserved as a failed durable identity.
			_ = r.reg.Close(id)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		row.Phase, row.Error = MemberFailed, err.Error()
		r.members[row.ID] = row
		return MemberView{MemberSnapshot: row, Role: "teammate", Status: agent.StatusClosed}, err
	}
	row.Phase = MemberActive
	r.members[row.ID] = row
	r.handles[row.ID] = handle
	return MemberView{MemberSnapshot: row, Role: "teammate", Status: handle.Status()}, nil
}

// Authorize checks that actor is a roster member and, when target is set, that
// it is addressing a member in this Team. Callers still apply operation-level
// rules (for example, only the lead can reassign a task).
func (r *Roster) Authorize(actor, target agent.ID) error {
	if r == nil {
		return ErrMemberNotFound
	}
	r.mu.Lock()
	actorRow, actorOK := r.members[string(actor)]
	targetRow, targetOK := r.members[string(target)]
	r.mu.Unlock()
	actorOK = actorOK && (actor == r.leadID || actorRow.Phase == MemberActive)
	targetOK = targetOK && (target == r.leadID || targetRow.Phase == MemberActive)
	if !actorOK || (target != "" && !targetOK) {
		return ErrUnauthorized
	}
	return nil
}

// Member returns a current detached roster view.
func (r *Roster) Member(id agent.ID) (MemberView, error) {
	r.mu.Lock()
	row, ok := r.members[string(id)]
	h := r.handles[string(id)]
	r.mu.Unlock()
	if !ok {
		return MemberView{}, ErrMemberNotFound
	}
	status := agent.StatusClosed
	if h != nil {
		status = h.Status()
	}
	role := "teammate"
	if id == r.leadID {
		role = "lead"
	}
	return MemberView{MemberSnapshot: row, Role: role, Status: status}, nil
}

// ResolveTarget accepts the stable member id or the model-facing immutable
// name and returns only an active member. Failed/provisioning rows cannot be
// addressed by mailbox delivery.
func (r *Roster) ResolveTarget(target string) (MemberView, error) {
	if r == nil {
		return MemberView{}, ErrMemberNotFound
	}
	target = strings.TrimSpace(target)
	r.mu.Lock()
	ids := make([]string, 0, len(r.members))
	for id, row := range r.members {
		if id == target || row.Name == target {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		view, err := r.Member(agent.ID(id))
		if err == nil && (view.ID == string(r.leadID) || view.Phase == MemberActive) {
			return view, nil
		}
	}
	return MemberView{}, ErrMemberNotFound
}

// Handle returns the live Agent handle for an active member. Durable rows do
// not fabricate handles after restore, so callers must explicitly reprovision
// a cold member before using this method.
func (r *Roster) Handle(id agent.ID) (*agent.Handle, error) {
	if r == nil {
		return nil, ErrMemberNotFound
	}
	r.mu.Lock()
	h := r.handles[string(id)]
	r.mu.Unlock()
	if h == nil {
		return nil, ErrMemberNotFound
	}
	return h, nil
}

// Rebind attaches a live Agent handle to an already-restored active member.
// It is the restart seam for durable Teams: identity, parent lineage and the
// name reservation come from the snapshot, while the runner is supplied by
// the composition root for the current process. Failed rebinds are retained
// as failed rows and never silently reported as active.
func (r *Roster) Rebind(ctx context.Context, id agent.ID, runner agent.Runner) (MemberView, error) {
	if r == nil {
		return MemberView{}, ErrMemberNotFound
	}
	if id == "" || runner == nil {
		return MemberView{}, errors.New("team: member id and runner are required")
	}
	r.mu.Lock()
	row, ok := r.members[string(id)]
	_, alreadyBound := r.handles[string(id)]
	r.mu.Unlock()
	if !ok {
		return MemberView{}, ErrMemberNotFound
	}
	if id == r.leadID || row.Phase != MemberActive {
		return MemberView{}, errors.New("team: only an unbound active teammate may be rebound")
	}
	if alreadyBound {
		return r.Member(id)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	handle, err := r.reg.Create(agent.Options{ID: id, ParentID: r.leadID, Runner: runner})
	if err == nil {
		err = handle.Start(ctx)
		if err != nil {
			_ = r.reg.Close(id)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		row.Phase = MemberFailed
		row.Error = err.Error()
		r.members[string(id)] = row
		return MemberView{MemberSnapshot: row, Role: "teammate", Status: agent.StatusClosed}, err
	}
	r.handles[string(id)] = handle
	return MemberView{MemberSnapshot: row, Role: "teammate", Status: handle.Status()}, nil
}

// RebindProvisioning recovers a child whose provisioning record was durable
// before the process stopped. The child identity is never replaced: a fresh
// Agent handle is attached to the same id and the row becomes active only
// after Start succeeds. The caller persists the resulting active edge.
func (r *Roster) RebindProvisioning(ctx context.Context, id agent.ID, runner agent.Runner) (MemberView, error) {
	if r == nil {
		return MemberView{}, ErrMemberNotFound
	}
	if id == "" || runner == nil {
		return MemberView{}, errors.New("team: member id and runner are required")
	}
	r.mu.Lock()
	row, ok := r.members[string(id)]
	_, alreadyBound := r.handles[string(id)]
	r.mu.Unlock()
	if !ok {
		return MemberView{}, ErrMemberNotFound
	}
	if id == r.leadID || row.Phase != MemberProvisioning {
		return MemberView{}, errors.New("team: only a provisioning teammate may be rebound")
	}
	if alreadyBound {
		return MemberView{}, errors.New("team: provisioning teammate already has a live handle")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	handle, err := r.reg.Create(agent.Options{ID: id, ParentID: r.leadID, Runner: runner})
	if err == nil {
		err = handle.Start(ctx)
		if err != nil {
			_ = r.reg.Close(id)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		row.Phase = MemberFailed
		row.Error = err.Error()
		r.members[string(id)] = row
		return MemberView{MemberSnapshot: row, Role: "teammate", Status: agent.StatusClosed}, err
	}
	row.Phase = MemberActive
	row.Error = ""
	r.members[string(id)] = row
	r.handles[string(id)] = handle
	return MemberView{MemberSnapshot: row, Role: "teammate", Status: handle.Status()}, nil
}

// MarkFailed records a cold-recovery failure without fabricating a live Agent.
// It is used when the durable child Session or lineage has disappeared before
// the process can call Rebind.
func (r *Roster) MarkFailed(id agent.ID, cause error) error {
	if r == nil {
		return ErrMemberNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.members[string(id)]
	if !ok {
		return ErrMemberNotFound
	}
	if id == r.leadID {
		return ErrNotLead
	}
	row.Phase = MemberFailed
	if cause != nil {
		row.Error = cause.Error()
	}
	r.members[string(id)] = row
	delete(r.handles, string(id))
	return nil
}

// ApplyMemberEvent folds one durable member record without creating a live
// Agent. Runtime handles are intentionally attached only by Rebind after the
// complete session log has been validated.
func (r *Roster) ApplyMemberEvent(row MemberSnapshot) error {
	if r == nil {
		return ErrMemberNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applyMemberEventLocked(row)
}

// applyMemberEventLocked folds a durable member row. Callers that also own a
// Board may hold the Board->Roster lock order and use this helper to commit
// both projections as one in-process state transition.
func (r *Roster) applyMemberEventLocked(row MemberSnapshot) error {
	if err := r.validateMemberEventLocked(row); err != nil {
		return err
	}
	r.members[row.ID] = row
	return nil
}

func (r *Roster) validateMemberEventLocked(row MemberSnapshot) error {
	if row.ID == "" || row.Name == "" || (row.Phase != MemberProvisioning && row.Phase != MemberActive && row.Phase != MemberFailed) {
		return errors.New("team: invalid member event")
	}
	if row.Name != "lead" && (!memberNamePattern.MatchString(row.Name) || len(row.Name) > 64) {
		return errors.New("team: invalid roster member name")
	}
	prior, exists := r.members[row.ID]
	if !exists {
		if row.ID == string(r.leadID) {
			return errors.New("team: member event changed lead identity")
		}
		if len(r.members)-1 >= r.maxMembers {
			return fmt.Errorf("team: member limit %d reached", r.maxMembers)
		}
		for _, member := range r.members {
			if member.Name == row.Name && member.ID != row.ID {
				return ErrMemberExists
			}
		}
		if row.Phase != MemberProvisioning {
			return errors.New("team: a new member must begin provisioning")
		}
		return nil
	}
	if prior.Name != row.Name || prior.Description != row.Description || prior.Provider != row.Provider || prior.Context != row.Context {
		return errors.New("team: member event changed immutable identity fields")
	}
	if prior.Phase == MemberProvisioning {
		if row.Phase == MemberProvisioning {
			return errors.New("team: duplicate member provisioning event")
		}
	} else if prior.Phase == MemberActive && row.Phase != MemberFailed {
		return errors.New("team: member event follows active member state")
	} else if prior.Phase == MemberFailed {
		return errors.New("team: member event follows terminal member state")
	}
	return nil
}

// List returns deterministic roster views. Failed and provisioning members are
// retained because their identities are part of the durable Team contract.
func (r *Roster) List() []MemberView {
	r.mu.Lock()
	ids := make([]string, 0, len(r.members))
	for id := range r.members {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	// Avoid exposing map iteration order; IDs are stable and opaque.
	sortStrings(ids)
	out := make([]MemberView, 0, len(ids))
	for _, id := range ids {
		if member, err := r.Member(agent.ID(id)); err == nil {
			out = append(out, member)
		}
	}
	return out
}

// Snapshot returns detached durable roster rows without live handles.
func (r *Roster) Snapshot() []MemberSnapshot {
	r.mu.Lock()
	ids := make([]string, 0, len(r.members))
	for id := range r.members {
		ids = append(ids, id)
	}
	sortStrings(ids)
	out := make([]MemberSnapshot, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.members[id])
	}
	r.mu.Unlock()
	return out
}

// Restore replaces only durable rows. It never fabricates a live Agent; a
// caller must explicitly reprovision each teammate and then transition it to
// active, which prevents cold restore from claiming work without a runtime.
func (r *Roster) Restore(rows []MemberSnapshot) error {
	if r == nil {
		return ErrMemberNotFound
	}
	members := make(map[string]MemberSnapshot, len(rows))
	names := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ID == "" || row.Name == "" || (row.Phase != MemberProvisioning && row.Phase != MemberActive && row.Phase != MemberFailed) {
			return errors.New("team: invalid roster snapshot")
		}
		if row.Name != "lead" && (!memberNamePattern.MatchString(row.Name) || len(row.Name) > 64) {
			return errors.New("team: invalid roster member name")
		}
		if _, exists := members[row.ID]; exists {
			return ErrMemberExists
		}
		if prior, exists := names[row.Name]; exists && prior != row.ID {
			return ErrMemberExists
		}
		if row.ID != string(r.leadID) && len(members) >= r.maxMembers+1 {
			return errors.New("team: roster member limit exceeded")
		}
		names[row.Name] = row.ID
		members[row.ID] = row
	}
	lead, ok := members[string(r.leadID)]
	if !ok || lead.Name != "lead" || lead.Phase != MemberActive {
		return errors.New("team: roster snapshot lacks active lead")
	}
	r.mu.Lock()
	r.members = members
	r.handles = make(map[string]*agent.Handle)
	r.mu.Unlock()
	return nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
