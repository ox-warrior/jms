package cli

import (
	"strings"
	"testing"
)

// Both defaults are empty in the source tree and only ever filled by an
// -ldflags build. These two tests are what stops a deployment from injecting
// the values that were wrong before: they assert nothing about the empty case,
// and everything about a non-empty one.

// The built-in --server default must be reachable from outside whatever network
// JumpServer happens to sit in. It used to be an internal IP, which timed out
// for everyone not on that network and looked like Koko being down.
func TestDefaultServerIsNotAnInternalIP(t *testing.T) {
	if defaultServer == "" {
		return // stock build: requireAddresses makes the user supply one
	}
	if strings.HasPrefix(defaultServer, "172.") || strings.HasPrefix(defaultServer, "10.") ||
		strings.HasPrefix(defaultServer, "192.168.") {
		t.Errorf("defaultServer = %q: a private IP only resolves from one network", defaultServer)
	}
	if !strings.Contains(defaultServer, ".") {
		t.Errorf("defaultServer = %q: want a hostname", defaultServer)
	}
}

// The jmsd default must name the deployed instance, not localhost. localhost
// only works for a developer running jmsd alongside jms; shipped to everyone
// else it makes a fresh install fail until a config file is written.
func TestDefaultJmsdAddrIsNotLocalhost(t *testing.T) {
	if defaultJmsdAddr == "" {
		return // stock build: requireAddresses makes the user supply one
	}
	if strings.Contains(defaultJmsdAddr, "localhost") || strings.Contains(defaultJmsdAddr, "127.0.0.1") {
		t.Errorf("defaultJmsdAddr = %q: only right on a developer's own machine", defaultJmsdAddr)
	}
	if !strings.HasPrefix(defaultJmsdAddr, "http://") && !strings.HasPrefix(defaultJmsdAddr, "https://") {
		t.Errorf("defaultJmsdAddr = %q: want a full base URL with scheme", defaultJmsdAddr)
	}
}

// A stock build has no addresses compiled in, so the run has to stop before the
// TUI takes the screen -- the failure mode this replaced was a full-screen UI
// erroring on its first request with the terminal already handed over.
func TestRequireAddresses(t *testing.T) {
	for _, tc := range []struct {
		name, server, jmsd string
		tunnelOnly         bool
		wantErr            bool
	}{
		{name: "both set", server: "jumpserver.example.com", jmsd: "http://jmsd.example.com:8391"},
		{name: "no server", jmsd: "http://jmsd.example.com:8391", wantErr: true},
		{name: "no jmsd", server: "jumpserver.example.com", wantErr: true},
		{name: "neither", wantErr: true},
		{name: "whitespace only", server: "  ", jmsd: "\t", wantErr: true},
		// "-w" never dials Koko, so it must start without --server -- but it
		// still cannot start without jmsd, which is where it learns the
		// tunnel's address and host key.
		{name: "tunnel needs no server", jmsd: "http://jmsd.example.com:8391", tunnelOnly: true},
		{name: "tunnel still needs jmsd", server: "jumpserver.example.com", tunnelOnly: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireAddresses(tc.server, tc.jmsd, "/tmp/jms/config.yaml", tc.tunnelOnly)
			if (err != nil) != tc.wantErr {
				t.Fatalf("requireAddresses(%q, %q, tunnelOnly=%t) error = %v, want error = %t",
					tc.server, tc.jmsd, tc.tunnelOnly, err, tc.wantErr)
			}
			// The whole point of the message is telling the user which file to
			// write; an error that omits the path sends them to --help instead.
			if err != nil && !strings.Contains(err.Error(), "/tmp/jms/config.yaml") {
				t.Errorf("error %q does not name the config file to create", err)
			}
		})
	}
}

// Both addresses must stay overridable from the config file, so that a
// deployment can be set up once per machine instead of typed on every run.
func TestAddressFlagsHaveConfigFileFallback(t *testing.T) {
	cmd := newRootCommand()
	for _, name := range []string{"server", "jmsd-addr"} {
		flag := cmd.PersistentFlags().Lookup(name)
		if flag == nil {
			t.Fatalf("--%s must exist", name)
		}
		if !strings.Contains(flag.Usage, "config file") {
			t.Errorf("--%s help %q should say the config file can set it", name, flag.Usage)
		}
		// Precedence is decided by Changed, not by comparing against the
		// default -- that is the only way to tell "not passed" from "passed
		// a value that happens to equal the default".
		if flag.Changed {
			t.Errorf("--%s reports Changed before parsing", name)
		}
	}
}

func TestK8sIsLocalOnly(t *testing.T) {
	cmd := newRootCommand()
	if cmd.PersistentFlags().Lookup("k8s-mode") != nil {
		t.Fatal("--k8s-mode must not exist: k8s always runs locally")
	}
	if cmd.PersistentFlags().Lookup("k9s-path") == nil {
		t.Fatal("--k9s-path must remain available for the local k9s binary")
	}
}

// "-w" and "-c" are two modes, not a combination: accepting both would have to
// silently drop one. The mistake it catches is real -- someone who typed a
// shortcut and then decided to leave a proxy up, or vice versa, gets the first
// one they typed and no hint that the other was ignored.
func TestValidateTunnelFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		port    int
		command string
		wantErr bool
	}{
		{name: "no tunnel"},
		{name: "tunnel only", port: 9090},
		{name: "tunnel with -c", port: 9090, command: "ec2 ng0", wantErr: true},
		{name: "kind-only -c still conflicts", port: 9090, command: "ec2", wantErr: true},
		{name: "port too high", port: 70000, wantErr: true},
		{name: "negative port", port: -1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTunnelFlags(rootFlags{tunnelPort: tc.port, command: tc.command})
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateTunnelFlags(port=%d, -c %q) error = %v, want error = %t",
					tc.port, tc.command, err, tc.wantErr)
			}
		})
	}
}
