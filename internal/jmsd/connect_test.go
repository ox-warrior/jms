package jmsd

import (
	"testing"

	"github.com/jumpserver-dev/sdk-go/model"
)

func TestBuildTokenRequestUsesVerifiedUserID(t *testing.T) {
	user := &userRecord{ID: "verified-user-id", Username: "alice"}
	req := buildTokenRequest(user, "asset-1", "root", "ssh", "10.0.0.9:51234")
	if req.UserId != user.ID {
		t.Fatalf("UserId = %q, want %q (must always come from the hop①-verified record, never client input)", req.UserId, user.ID)
	}
	if req.AssetId != "asset-1" || req.Account != "root" || req.Protocol != "ssh" {
		t.Fatalf("unexpected token request: %+v", req)
	}
	// Forwarded so Core's own connection-token record names the caller's
	// machine. Left blank, jmsd's log was the only place a ticket could be
	// traced to a source and the JumpServer console showed none at all.
	if req.RemoteAddr != "10.0.0.9:51234" {
		t.Errorf("RemoteAddr = %q, want the caller's address forwarded to Core", req.RemoteAddr)
	}
	if req.ConnectMethod == "" {
		t.Fatal("ConnectMethod must be non-empty: the field has no blank=True on the Core side")
	}
}

func TestFilterKokoProtocols(t *testing.T) {
	in := []model.Protocol{
		{Name: "ssh", Port: 22},
		{Name: "rdp", Port: 3389},
		{Name: "MySQL", Port: 3306},
		{Name: "vnc", Port: 5900},
	}
	out := filterKokoProtocols(in)
	if len(out) != 2 {
		t.Fatalf("filterKokoProtocols(%v) = %v, want 2 koko-compatible entries", in, out)
	}
	if out[0].Name != "ssh" || out[1].Name != "MySQL" {
		t.Fatalf("unexpected protocols kept: %+v", out)
	}
}

func TestToAccountOptionsExcludesInput(t *testing.T) {
	in := []model.PermAccount{
		{Alias: "root", Name: "root", Username: "root"},
		{Alias: inputAccountAlias, Name: "manual", Username: ""},
		{Alias: "@ANON", Name: "anonymous", Username: ""},
	}
	out := toAccountOptions(in)
	if len(out) != 2 {
		t.Fatalf("toAccountOptions(%v) = %v, want @INPUT filtered out", in, out)
	}
	for _, a := range out {
		if a.Alias == inputAccountAlias {
			t.Fatalf("inputAccountAlias leaked through: %+v", out)
		}
	}
}
