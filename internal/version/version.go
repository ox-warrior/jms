// Package version is the one place this repo's version numbers are written
// down, so the client, the daemon and the strings shown to users cannot drift
// apart from each other.
//
// Before this existed, the version lived in internal/cli and the JumpServer
// baseline was spelled out separately in two user-facing places (the
// `jms version` output and the TUI's own startup panel title) with nothing
// keeping them in step.
package version

// Version is the version of this build -- of BOTH binaries. jms and jmsd are
// built from one repo and released together, which is what makes the update
// check in the TUI meaningful: jmsd reports this value over /v1/whoami, jms
// compares it against its own compiled-in copy, and any difference means the
// two binaries came from different builds. A laptop running a stale jms is
// exactly that case.
//
// Injected by scripts/release.sh from the git tag:
//
//	go build -ldflags "-X .../internal/version.Version=1.2.3"
//
// It used to be a hand-edited constant, which put the release process one
// forgotten edit away from shipping a binary that lies about its own version --
// and since the update check compares exactly this value against the published
// manifest, a stale number means the offer either never appears or reappears on
// every launch after updating.
//
// The "dev" default is deliberately not a version number: isNewer fails to
// parse it, Check returns that as an error, and startUpdateCheck logs it at
// debug and moves on. A `go build` straight from a clone therefore never offers
// to replace itself with a release, which is right -- the local tree is not
// behind the release, it is unrelated to it.
var Version = "dev"

// JumpServerBaseline is the JumpServer/Koko release THIS CLIENT was developed
// and verified against.
//
// It is deliberately NOT presented as jms's own version. jms has its own
// release cycle; this is a compatibility note about what it was tested with,
// which is a different fact and belongs in smaller print. It is also only ever
// an assertion -- nothing here checks that the JumpServer on the other end
// really is this release.
//
// jmsd declares its own baseline separately rather than sharing this one (see
// jmsd.JumpServerBaseline), and reports it over /v1/whoami so the two can be
// compared across the wire. Two declarations, not one shared constant, is the
// point: it lets the two halves disagree, which is the only way a difference
// can ever be reported.
const JumpServerBaseline = "v4.10.16"
