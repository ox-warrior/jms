package connectflow

import (
	"testing"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

func TestIsK8sProtocol(t *testing.T) {
	// Koko is not consistent about the casing it reports, and getting this
	// wrong sends a cluster down the interactive-shell path instead of k9s.
	for _, name := range []string{"k8s", "K8S", "K8s"} {
		if !IsK8sProtocol(name) {
			t.Errorf("IsK8sProtocol(%q) = false, want true -- must use the local k9s path", name)
		}
	}
	if IsK8sProtocol("ssh") {
		t.Error("IsK8sProtocol(\"ssh\") = true, want false -- must keep using the SSH shell path")
	}
}

func TestIsK8sAsset(t *testing.T) {
	cases := []struct {
		category, typ string
		want          bool
	}{
		{"cloud", "k8s", true},
		{"CLOUD", "K8S", true}, // case-insensitive, matching JumpServer's own inconsistency
		{"cloud", "vm", false},
		{"host", "k8s", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := IsK8sAsset(c.category, c.typ); got != c.want {
			t.Errorf("IsK8sAsset(%q, %q) = %t, want %t", c.category, c.typ, got, c.want)
		}
	}
}

func TestRequireK8sProtocol(t *testing.T) {
	k8s := jmsd.ProtocolOption{Name: "k8s", Port: 6443}
	ssh := jmsd.ProtocolOption{Name: "ssh", Port: 22}

	got, err := RequireK8sProtocol([]jmsd.ProtocolOption{ssh, k8s})
	if err != nil || got != k8s {
		t.Errorf("RequireK8sProtocol([ssh,k8s]) = (%v, %v), want (%v, nil)", got, err, k8s)
	}

	// Casing must not decide whether a cluster is reachable.
	upper := jmsd.ProtocolOption{Name: "K8S", Port: 6443}
	if got, err := RequireK8sProtocol([]jmsd.ProtocolOption{ssh, upper}); err != nil || got != upper {
		t.Errorf("RequireK8sProtocol([ssh,K8S]) = (%v, %v), want (%v, nil)", got, err, upper)
	}

	if _, err := RequireK8sProtocol([]jmsd.ProtocolOption{ssh}); err == nil {
		t.Error("RequireK8sProtocol([ssh]) = nil error, want an error -- a k8s asset with no k8s protocol is a jmsd bug")
	}

	if _, err := RequireK8sProtocol(nil); err == nil {
		t.Error("RequireK8sProtocol(nil) = nil error, want an error")
	}
}

func TestSelectProtocol(t *testing.T) {
	ssh := jmsd.ProtocolOption{Name: "ssh"}
	sftp := jmsd.ProtocolOption{Name: "sftp"}
	mysql := jmsd.ProtocolOption{Name: "mysql"}

	t.Run("ssh wins even when offered alongside others", func(t *testing.T) {
		got, ok := SelectProtocol([]jmsd.ProtocolOption{sftp, ssh, mysql})
		if !ok || got != ssh {
			t.Errorf("SelectProtocol([sftp,ssh,mysql]) = (%v, %t), want (%v, true)", got, ok, ssh)
		}
	})

	t.Run("the sole option is picked when there is no ssh", func(t *testing.T) {
		got, ok := SelectProtocol([]jmsd.ProtocolOption{mysql})
		if !ok || got != mysql {
			t.Errorf("SelectProtocol([mysql]) = (%v, %t), want (%v, true)", got, ok, mysql)
		}
	})

	t.Run("ambiguous without ssh reports ok=false", func(t *testing.T) {
		_, ok := SelectProtocol([]jmsd.ProtocolOption{mysql, sftp})
		if ok {
			t.Error("SelectProtocol([mysql,sftp]) = ok=true, want false -- neither is an obvious default")
		}
	})

	t.Run("empty options reports ok=false", func(t *testing.T) {
		_, ok := SelectProtocol(nil)
		if ok {
			t.Error("SelectProtocol(nil) = ok=true, want false")
		}
	})
}
