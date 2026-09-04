package config

import "testing"

func TestResolveServer(t *testing.T) {
	t.Parallel()
	ep, err := ResolveServer("jumpserver.example.com", 2222)
	if err != nil {
		t.Fatal(err)
	}
	if ep.KokoAddr != "jumpserver.example.com:2222" {
		t.Fatalf("KokoAddr = %q", ep.KokoAddr)
	}
}

func TestResolveServerRejectsPath(t *testing.T) {
	t.Parallel()
	if _, err := ResolveServer("https://jumpserver.example.com/ui", 2222); err == nil {
		t.Fatal("expected URL path to be rejected")
	}
}

// A scheme written into --server is parsed off and discarded: the only thing
// derived from it is an SSH address, and SSH has no scheme. This replaced a
// test for the removed --scheme flag, which validated a value nothing read.
func TestResolveServerIgnoresScheme(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"jumpserver.example.com",
		"https://jumpserver.example.com",
		"http://jumpserver.example.com",
	} {
		ep, err := ResolveServer(raw, 2222)
		if err != nil {
			t.Fatalf("ResolveServer(%q) error = %v", raw, err)
		}
		if ep.KokoAddr != "jumpserver.example.com:2222" {
			t.Errorf("ResolveServer(%q).KokoAddr = %q, want jumpserver.example.com:2222", raw, ep.KokoAddr)
		}
	}
}

// The port in --server is Core's, which jms reaches through jmsd and never
// dials itself; Koko's port comes from --ssh-port and nowhere else.
func TestResolveDevelopmentServer(t *testing.T) {
	t.Parallel()
	ep, err := ResolveServer("localhost:8080", 2200)
	if err != nil {
		t.Fatal(err)
	}
	if ep.KokoAddr != "localhost:2200" {
		t.Fatalf("KokoAddr = %q, want localhost:2200", ep.KokoAddr)
	}
}
