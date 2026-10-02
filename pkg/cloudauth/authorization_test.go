package cloudauth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"miren.dev/runtime/pkg/auth"
	"miren.dev/runtime/pkg/rbac"
	"miren.dev/runtime/pkg/rpc"
	"miren.dev/runtime/pkg/uplink"
)

func authorizationSession(id string) uplink.Session {
	return uplink.Session{ID: id, OrganizationID: "org-1", Capabilities: []uplink.CapabilitySelection{{Name: AuthorizationCapability, Version: 1}}}
}

func authorizationSnapshot(session string, revision uint64) AuthorizationSnapshot {
	return AuthorizationSnapshot{
		SessionID: session, OrganizationID: "org-1", Revision: revision,
		Policy:      rbac.Policy{Rules: []rbac.Rule{{Name: "read apps", Groups: []string{"readers"}, Permissions: []rbac.Permission{{Resource: "apps/*", Actions: []string{"read"}}}}}},
		Memberships: map[string][]string{"alice": {"z-other", "readers"}, "bob": {}},
	}
}

func deliverSnapshot(t *testing.T, state *AuthorizationState, snapshot AuthorizationSnapshot) {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NoError(t, state.receiveSnapshot(t.Context(), raw))
}

func TestAuthorizationSnapshots(t *testing.T) {
	s := NewAuthorizationState(t.Context(), slog.Default())
	evaluate := func(subject, org string) rbac.Decision {
		return s.Evaluate(&rbac.Request{Subject: subject, Groups: []string{"readers"}, Resource: "apps/demo", Action: "read"}, org)
	}
	require.Equal(t, rbac.DecisionDeny, evaluate("alice", "org-1"))
	ctx, cancel := context.WithCancel(t.Context())
	s.beginSession(ctx, authorizationSession("first"))
	require.Equal(t, rbac.DecisionDeny, evaluate("alice", "org-1"))
	snapshot := authorizationSnapshot("first", 1)
	deliverSnapshot(t, s, snapshot)
	require.Equal(t, rbac.DecisionAllow, evaluate("alice", "org-1"))
	require.Equal(t, rbac.DecisionDeny, evaluate("bob", "org-1"))
	require.Equal(t, rbac.DecisionDeny, evaluate("missing", "org-1"))
	require.Equal(t, rbac.DecisionDeny, evaluate("alice", "other-org"))
	require.Equal(t, rbac.DecisionDeny, evaluate("alice", ""))
	require.Equal(t, []string{"z-other", "readers"}, s.snapshot.Memberships["alice"])

	// Same user/groups/request, different rules: a cached allow must be revoked.
	snapshot.Revision++
	snapshot.Policy.Rules = []rbac.Rule{}
	deliverSnapshot(t, s, snapshot)
	require.Equal(t, rbac.DecisionDeny, evaluate("alice", "org-1"))
	snapshot = authorizationSnapshot("first", 3)
	deliverSnapshot(t, s, snapshot)
	require.Equal(t, rbac.DecisionAllow, evaluate("alice", "org-1"))
	snapshot.Revision++
	snapshot.Memberships["alice"] = []string{}
	deliverSnapshot(t, s, snapshot)
	require.Equal(t, rbac.DecisionDeny, evaluate("alice", "org-1"))
	snapshot = authorizationSnapshot("first", 5)
	deliverSnapshot(t, s, snapshot)
	require.Equal(t, rbac.DecisionAllow, evaluate("alice", "org-1"))
	snapshot.Revision++
	delete(snapshot.Memberships, "alice")
	deliverSnapshot(t, s, snapshot)
	require.Equal(t, rbac.DecisionDeny, evaluate("alice", "org-1"))

	deliverSnapshot(t, s, authorizationSnapshot("first", 7))
	require.Equal(t, rbac.DecisionAllow, evaluate("alice", "org-1"))
	cancel()
	require.Equal(t, rbac.DecisionDeny, evaluate("alice", "org-1"))
	s.beginSession(t.Context(), authorizationSession("second"))
	require.Equal(t, rbac.DecisionDeny, evaluate("alice", "org-1"))
	// Reconnect resets the revision and receives changes missed while offline.
	snapshot = authorizationSnapshot("second", 1)
	snapshot.Memberships = map[string][]string{"bob": {"readers"}}
	deliverSnapshot(t, s, snapshot)
	require.Equal(t, rbac.DecisionDeny, evaluate("alice", "org-1"))
	require.Equal(t, rbac.DecisionAllow, evaluate("bob", "org-1"))
}

func TestAuthorizationLocalTagSelectorsAndEmptyState(t *testing.T) {
	s := NewAuthorizationState(t.Context(), slog.Default())
	s.beginSession(t.Context(), authorizationSession("session"))
	snapshot := authorizationSnapshot("session", 1)
	snapshot.Policy.Rules[0].TagSelector.Expressions = []rbac.TagExpression{{Tag: "environment", Operator: "equals", Value: "production"}}
	deliverSnapshot(t, s, snapshot)
	for _, tc := range []struct {
		environment string
		want        rbac.Decision
	}{
		{"production", rbac.DecisionAllow},
		{"development", rbac.DecisionDeny},
	} {
		require.Equal(t, tc.want, s.Evaluate(&rbac.Request{Subject: "alice", Resource: "apps/demo", Action: "read", Tags: map[string]any{"environment": tc.environment}}, "org-1"))
	}
	snapshot.Revision++
	snapshot.Policy.Rules = []rbac.Rule{}
	snapshot.Memberships = map[string][]string{}
	deliverSnapshot(t, s, snapshot)
	require.Equal(t, rbac.DecisionDeny, s.Evaluate(&rbac.Request{Subject: "alice", Resource: "apps/demo", Action: "read", Tags: map[string]any{"environment": "production"}}, "org-1"))
}

func TestAuthorizationRejectsInvalidSnapshots(t *testing.T) {
	s := NewAuthorizationState(t.Context(), slog.Default())
	s.beginSession(t.Context(), authorizationSession("session"))
	deliverSnapshot(t, s, authorizationSnapshot("session", 2))
	for _, tc := range []struct {
		name   string
		mutate func(*AuthorizationSnapshot)
	}{
		{"wrong session", func(s *AuthorizationSnapshot) { s.SessionID = "old" }},
		{"wrong organization", func(s *AuthorizationSnapshot) { s.OrganizationID = "other" }},
		{"old revision", func(s *AuthorizationSnapshot) { s.Revision = 1 }},
		{"duplicate", func(s *AuthorizationSnapshot) { s.Revision = 2 }},
		{"zero revision", func(s *AuthorizationSnapshot) { s.Revision = 0 }},
		{"missing memberships", func(s *AuthorizationSnapshot) { s.Memberships = nil }},
		{"missing rules", func(s *AuthorizationSnapshot) { s.Policy.Rules = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := authorizationSnapshot("session", 3)
			tc.mutate(&snapshot)
			raw, err := json.Marshal(snapshot)
			require.NoError(t, err)
			require.Error(t, s.receiveSnapshot(t.Context(), raw))
			require.Equal(t, uint64(2), s.snapshot.Revision)
		})
	}
	require.Error(t, s.receiveSnapshot(t.Context(), json.RawMessage(`{`)))
	s.beginSession(t.Context(), uplink.Session{ID: "unselected", OrganizationID: "org-1"})
	raw, err := json.Marshal(authorizationSnapshot("unselected", 1))
	require.NoError(t, err)
	require.Error(t, s.receiveSnapshot(t.Context(), raw))
}

func TestAuthorizationConcurrentRevocation(t *testing.T) {
	s := NewAuthorizationState(t.Context(), slog.Default())
	s.beginSession(t.Context(), authorizationSession("session"))
	deliverSnapshot(t, s, authorizationSnapshot("session", 1))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				s.Evaluate(&rbac.Request{Subject: "alice", Resource: "apps/demo", Action: "read"}, "org-1")
			}
		})
	}
	snapshot := authorizationSnapshot("session", 2)
	snapshot.Policy.Rules = []rbac.Rule{}
	deliverSnapshot(t, s, snapshot)
	wg.Wait()
	for range 100 {
		require.Equal(t, rbac.DecisionDeny, s.Evaluate(&rbac.Request{Subject: "alice", Resource: "apps/demo", Action: "read"}, "org-1"))
	}
}

func TestRPCUsesPushedMembershipsWithoutHTTP(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a, err := NewRPCAuthenticator(t.Context(), Config{CloudURL: srv.URL, Logger: slog.Default()})
	require.NoError(t, err)
	a.authorization.beginSession(t.Context(), authorizationSession("session"))
	deliverSnapshot(t, a.authorization, authorizationSnapshot("session", 1))
	// A validated cached token can have stale groups; only its identity matters.
	a.tokenCache.Set("cached-token", &auth.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "alice"}, OrganizationID: "org-1", GroupIDs: []string{"not-readers"}})
	identity, err := a.Authenticate(t.Context(), &rpc.Credentials{Authorization: "Bearer cached-token"})
	require.NoError(t, err)
	require.Equal(t, []string{"not-readers"}, identity.Groups)
	require.NoError(t, a.Authorize(t.Context(), identity, "apps/demo", "read"))
	snapshot := authorizationSnapshot("session", 2)
	snapshot.Memberships["alice"] = []string{}
	deliverSnapshot(t, a.authorization, snapshot)
	identity.Groups = []string{"readers"}
	require.Error(t, a.Authorize(t.Context(), identity, "apps/demo", "read"))
	require.NoError(t, a.Authorize(t.Context(), &rpc.Identity{Method: rpc.AuthMethodCert}, "apps/demo", "read"))
	require.Zero(t, requests.Load(), "startup and authorization must not fetch cloud state")
}

func TestCloudEmittedAuthorizationEnvelope(t *testing.T) {
	// Captured from mirendev/cloud's real PostgreSQL + WebSocket emitter test
	// for MIR-2005, rather than marshaled from the runtime's own wire types.
	raw, err := os.ReadFile("testdata/authorization.snapshot.json")
	require.NoError(t, err)
	var envelope uplink.Envelope
	require.NoError(t, json.Unmarshal(raw, &envelope))
	for _, environment := range []string{"production", "development"} {
		t.Run(environment, func(t *testing.T) {
			a, err := NewRPCAuthenticator(t.Context(), Config{Logger: slog.Default(), Tags: map[string]any{"environment": environment}})
			require.NoError(t, err)
			router := uplink.NewMessageRouter()
			link := uplink.NewClient("https://unused.invalid", nil, router, slog.Default())
			a.RegisterAuthorization(link)
			session := authorizationSession("6779202f-d9c4-45d4-adbc-11623faf26c2")
			session.OrganizationID = "org-gd9ktl4kpl33"
			a.authorization.beginSession(t.Context(), session)
			require.NoError(t, router.Dispatch(t.Context(), envelope))
			identity := &rpc.Identity{Subject: "usr-l0668v4hhgcw", Method: rpc.AuthMethodJWT, Metadata: map[string]any{"organization_id": "org-gd9ktl4kpl33"}}
			for _, action := range []string{"read", "write", "delete"} {
				err := a.Authorize(t.Context(), identity, "apps/demo", action)
				if environment == "production" && action != "delete" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			}
		})
	}
}
