package cloudauth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"miren.dev/runtime/pkg/rbac"
	"miren.dev/runtime/pkg/uplink"
)

const (
	AuthorizationCapability   = "authorization"
	TypeAuthorizationSnapshot = "authorization.snapshot"
)

// AuthorizationSnapshot is the version 1 cloud authorization contract. Cloud
// sends full replacements, with increasing revisions within each session.
type AuthorizationSnapshot struct {
	SessionID      string              `json:"session_id"`
	OrganizationID string              `json:"organization_id"`
	Revision       uint64              `json:"revision"`
	Policy         rbac.Policy         `json:"policy"`
	Memberships    map[string][]string `json:"memberships"`
}

func (s *AuthorizationSnapshot) GetPolicy() *rbac.Policy { return &s.Policy }

// AuthorizationState serializes snapshot replacement with evaluation, including
// cache reads/writes. Clearing a cache alone would allow an in-flight old-policy
// evaluation to repopulate it after a revocation.
type AuthorizationState struct {
	mu         sync.Mutex
	logger     *slog.Logger
	snapshot   AuthorizationSnapshot
	evaluator  *rbac.Evaluator
	session    uplink.Session
	sessionCtx context.Context
	loaded     bool
}

func NewAuthorizationState(ctx context.Context, logger *slog.Logger) *AuthorizationState {
	s := &AuthorizationState{logger: logger.With("module", "authorization")}
	s.evaluator = rbac.NewEvaluator(ctx, &s.snapshot, logger)
	return s
}

type AuthorizationLink interface {
	OfferCapability(uplink.CapabilityOffer)
	OnSession(func(context.Context, uplink.Session))
	Handle(string, uplink.MessageHandler)
}

func (s *AuthorizationState) Register(link AuthorizationLink) {
	link.OfferCapability(uplink.CapabilityOffer{Name: AuthorizationCapability, Versions: []uint{1}})
	link.OnSession(s.beginSession)
	link.Handle(TypeAuthorizationSnapshot, s.receiveSnapshot)
}

func (s *AuthorizationState) beginSession(ctx context.Context, session uplink.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = false
	s.snapshot = AuthorizationSnapshot{}
	s.evaluator.ClearCache()
	s.session = session
	s.sessionCtx = ctx
}

func (s *AuthorizationState) receiveSnapshot(ctx context.Context, data json.RawMessage) error {
	var snapshot AuthorizationSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("invalid authorization snapshot: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	selection, selected := s.session.Capability(AuthorizationCapability)
	if !selected || selection.Version != 1 || s.sessionCtx == nil || s.sessionCtx.Err() != nil || ctx.Err() != nil {
		return fmt.Errorf("authorization snapshot outside a negotiated session")
	}
	if snapshot.SessionID != s.session.ID || snapshot.OrganizationID != s.session.OrganizationID {
		return fmt.Errorf("authorization snapshot scope does not match session")
	}
	if snapshot.Revision == 0 || snapshot.Revision <= s.snapshot.Revision {
		return fmt.Errorf("authorization snapshot revision is not increasing")
	}
	if snapshot.Memberships == nil || snapshot.Policy.Rules == nil {
		return fmt.Errorf("authorization snapshot must contain rules and memberships")
	}
	s.snapshot = snapshot
	s.loaded = true
	s.evaluator.ClearCache()
	s.logger.Info("cloud authorization snapshot applied", "revision", snapshot.Revision,
		"rules", len(snapshot.Policy.Rules), "users", len(snapshot.Memberships))
	return nil
}

func (s *AuthorizationState) Evaluate(req *rbac.Request, organizationID string) rbac.Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded || s.sessionCtx.Err() != nil || organizationID != s.snapshot.OrganizationID {
		return rbac.DecisionDeny
	}
	groups, present := s.snapshot.Memberships[req.Subject]
	if !present {
		return rbac.DecisionDeny
	}
	// The evaluator sorts request groups for its cache key; never lend it the
	// authoritative slice or the caller's token groups.
	req.Groups = slices.Clone(groups)
	return s.evaluator.Evaluate(req)
}
