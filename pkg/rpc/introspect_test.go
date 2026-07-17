package rpc

import (
	"context"
	"testing"
)

// TestHasMethodParam covers parameter-level capability detection: a client can
// tell a server that has a method from one that has a newer revision of it with
// an added parameter. This is what lets callers gate a flag like --until on
// actual `to` support instead of mere method presence.
func TestHasMethodParam(t *testing.T) {
	noop := func(ctx context.Context, call Call) error { return nil }
	iface := NewInterface([]Method{
		{
			Name:          "streamLogChunks",
			InterfaceName: "Logs",
			Params:        []string{"target", "from", "follow", "filter", "chunks", "to"},
			Handler:       noop,
		},
		{
			// Stands in for an older revision: method exists, no `to` param.
			Name:          "appLogs",
			InterfaceName: "Logs",
			Params:        []string{"application", "from", "follow"},
			Handler:       noop,
		},
	}, struct{}{})

	c := LocalClient(iface)
	ctx := context.Background()

	if !c.HasMethod(ctx, "streamLogChunks") {
		t.Fatal("expected streamLogChunks to be present")
	}
	if c.HasMethod(ctx, "missingMethod") {
		t.Error("unknown method must report absent")
	}
	if !c.HasMethodParam(ctx, "streamLogChunks", "to") {
		t.Error("expected streamLogChunks to advertise the 'to' param")
	}
	if c.HasMethodParam(ctx, "streamLogChunks", "bogus") {
		t.Error("a parameter the method does not declare must report false")
	}
	if c.HasMethodParam(ctx, "appLogs", "to") {
		t.Error("appLogs has no 'to' param; this is the old-server case the gate guards")
	}
	if c.HasMethodParam(ctx, "missingMethod", "to") {
		t.Error("unknown method must report false, not panic")
	}
}
