package connectflow

import (
	"errors"
	"strings"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

// IsK8sProtocol reports whether name is Koko's k8s protocol -- the one that
// hands the session to k9s (via internal/k8sclient) instead of an interactive
// shell.
func IsK8sProtocol(name string) bool {
	return strings.EqualFold(name, "k8s")
}

// IsK8sAsset reports whether an asset is a k8s cluster. It defers to
// jmsd.KindOf rather than re-testing category/type here, so this and the TUI's
// ":" filter cannot drift apart about what counts as a cluster.
func IsK8sAsset(category, typ string) bool {
	return jmsd.KindOf(category, typ) == jmsd.ResourceK8s
}

// RequireK8sProtocol finds the k8s entry in a k8s asset's protocol list. Its
// absence would mean jmsd's own filtering (docs/PLAN.md §5.A.5) let a k8s asset
// through without the one protocol it can ever be reached by -- a jmsd bug, not
// a user mistake, so this reports it rather than falling back to
// SelectProtocol.
func RequireK8sProtocol(options []jmsd.ProtocolOption) (jmsd.ProtocolOption, error) {
	for _, option := range options {
		if IsK8sProtocol(option.Name) {
			return option, nil
		}
	}
	return jmsd.ProtocolOption{}, errors.New("k8s asset has no k8s protocol")
}

// SelectProtocol deterministically picks ssh when it is offered -- which it
// usually is, alongside sftp, on the same account -- or the sole option when
// there is exactly one. ok is false when neither rule applies: more than one
// option is on offer and none is the obvious default, which is a choice only
// the caller's own policy can make (the TUI asks; "-c" refuses to guess).
func SelectProtocol(options []jmsd.ProtocolOption) (protocol jmsd.ProtocolOption, ok bool) {
	for _, p := range options {
		if p.Name == "ssh" {
			return p, true
		}
	}
	if len(options) == 1 {
		return options[0], true
	}
	return jmsd.ProtocolOption{}, false
}
