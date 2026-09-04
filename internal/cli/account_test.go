package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
)

var (
	root   = jmsd.AccountOption{Name: "root", Username: "root", Alias: "@root"}
	deploy = jmsd.AccountOption{Name: "deploy user", Username: "deploy", Alias: "@deploy"}
	ubuntu = jmsd.AccountOption{Name: "ubuntu", Username: "ubuntu", Alias: "@ubuntu"}
)

// refusePrompt fails the test if it is called at all -- used wherever the
// account is already determined and asking would be a bug.
func refusePrompt(t *testing.T) accountPrompt {
	t.Helper()
	return func(_ string, _ []jmsd.AccountOption) (int, error) {
		t.Fatal("the account was already determined; resolveAccount must not prompt")
		return 0, nil
	}
}

func TestResolveAccountUsesTheOnlyPermittedAccountWithoutAsking(t *testing.T) {
	got, err := resolveAccount("ng0", []jmsd.AccountOption{deploy}, "", refusePrompt(t))
	if err != nil || got != deploy {
		t.Fatalf("resolveAccount(one account) = (%v, %v), want (%v, nil)", got, err, deploy)
	}
}

func TestResolveAccountHonoursExplicitUsernameWithoutAsking(t *testing.T) {
	got, err := resolveAccount("ng0", []jmsd.AccountOption{root, deploy, ubuntu}, "deploy", refusePrompt(t))
	if err != nil || got != deploy {
		t.Fatalf("resolveAccount(--account deploy) = (%v, %v), want (%v, nil)", got, err, deploy)
	}
}

// The regression this whole function exists for: more than one permitted
// account must never resolve on its own. Picking options[0] made the same
// command connect as an ordinary account one day and a privileged one the
// next, since jmsd's ordering is not stable.
func TestResolveAccountNeverPicksAmongSeveralOnItsOwn(t *testing.T) {
	_, err := resolveAccount("ng0", []jmsd.AccountOption{root, deploy}, "", nil)
	if err == nil {
		t.Fatal("resolveAccount(2 accounts, no prompt) = nil error, want a refusal -- it must not pick one")
	}
	// The message has to say how to get unstuck, and name the choices.
	for _, want := range []string{"--account", "root", "deploy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestResolveAccountAsksWhenSeveralArePermitted(t *testing.T) {
	options := []jmsd.AccountOption{root, deploy, ubuntu}
	var sawAsset string
	var sawOptions []jmsd.AccountOption
	got, err := resolveAccount("ng0", options, "", func(asset string, opts []jmsd.AccountOption) (int, error) {
		sawAsset, sawOptions = asset, opts
		return 2, nil
	})
	if err != nil || got != ubuntu {
		t.Fatalf("resolveAccount(choice 2) = (%v, %v), want (%v, nil)", got, err, ubuntu)
	}
	if sawAsset != "ng0" {
		t.Errorf("prompt got asset %q, want %q -- it has to name what is being connected to", sawAsset, "ng0")
	}
	if len(sawOptions) != len(options) {
		t.Errorf("prompt got %d options, want all %d", len(sawOptions), len(options))
	}
}

func TestResolveAccountPropagatesACancelledChoice(t *testing.T) {
	cancelled := errors.New("account selection cancelled")
	_, err := resolveAccount("ng0", []jmsd.AccountOption{root, deploy}, "", func(string, []jmsd.AccountOption) (int, error) {
		return 0, cancelled
	})
	if !errors.Is(err, cancelled) {
		t.Fatalf("resolveAccount(cancelled) error = %v, want it to wrap %v", err, cancelled)
	}
}

// A prompt returning garbage must not index out of the slice.
func TestResolveAccountRejectsAnOutOfRangeChoice(t *testing.T) {
	for _, i := range []int{-1, 2, 99} {
		_, err := resolveAccount("ng0", []jmsd.AccountOption{root, deploy}, "", func(string, []jmsd.AccountOption) (int, error) {
			return i, nil
		})
		if err == nil {
			t.Errorf("resolveAccount(choice %d) = nil error, want a range refusal", i)
		}
	}
}

func TestResolveAccountRefusesAnUnmatchedUsername(t *testing.T) {
	_, err := resolveAccount("ng0", []jmsd.AccountOption{root, deploy}, "postgres", refusePrompt(t))
	if err == nil {
		t.Fatal("resolveAccount(--account postgres) = nil error, want a refusal")
	}
	if !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("error = %v, want it to wrap ErrAccountNotFound", err)
	}
	// Falling back to the prompt here would turn a typo into a connection.
	if !strings.Contains(err.Error(), "root") || !strings.Contains(err.Error(), "deploy") {
		t.Errorf("error %q does not list the candidates", err)
	}
}

func TestResolveAccountRefusesAnAmbiguousUsername(t *testing.T) {
	twin := jmsd.AccountOption{Name: "root (discovered)", Username: "root", Alias: "@root-2"}
	_, err := resolveAccount("ng0", []jmsd.AccountOption{root, twin}, "root", refusePrompt(t))
	if !errors.Is(err, ErrAccountAmbiguous) {
		t.Fatalf("resolveAccount(--account root, two matches) error = %v, want it to wrap ErrAccountAmbiguous", err)
	}
}

func TestPromptAccountChoiceReadsAOneBasedNumber(t *testing.T) {
	var out bytes.Buffer
	got, err := promptAccountChoice(strings.NewReader("2\n"), &out, "ng0", []jmsd.AccountOption{root, deploy, ubuntu})
	if err != nil || got != 1 {
		t.Fatalf("promptAccountChoice(\"2\") = (%d, %v), want (1, nil)", got, err)
	}
	// Every account has to be on screen with the number that selects it,
	// otherwise the number being typed is a guess.
	for _, want := range []string{"1) root", "2) deploy (deploy user)", "3) ubuntu", "account [1-3]:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("menu is missing %q; got:\n%s", want, out.String())
		}
	}
}

func TestPromptAccountChoiceReAsksUntilTheAnswerIsInRange(t *testing.T) {
	var out bytes.Buffer
	// Zero, a word, past the end, then a real answer.
	got, err := promptAccountChoice(strings.NewReader("0\nroot\n9\n1\n"), &out, "ng0",
		[]jmsd.AccountOption{root, deploy})
	if err != nil || got != 0 {
		t.Fatalf("promptAccountChoice(bad input then \"1\") = (%d, %v), want (0, nil)", got, err)
	}
	if n := strings.Count(out.String(), "not a choice"); n != 3 {
		t.Errorf("complained %d times, want 3 -- one per rejected answer", n)
	}
}

// Ctrl+D must end the run, not fall through to some account.
func TestPromptAccountChoiceTreatsEOFAsCancellation(t *testing.T) {
	var out bytes.Buffer
	_, err := promptAccountChoice(strings.NewReader(""), &out, "ng0", []jmsd.AccountOption{root, deploy})
	if err == nil {
		t.Fatal("promptAccountChoice(EOF) = nil error, want cancellation")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error = %v, want it to say the selection was cancelled", err)
	}
}

func TestDescribeAccountLeadsWithTheUsernameYouWouldType(t *testing.T) {
	if got := describeAccount(deploy); got != "deploy (deploy user)" {
		t.Errorf("describeAccount(deploy) = %q, want %q", got, "deploy (deploy user)")
	}
	// No point printing "root (root)".
	if got := describeAccount(root); got != "root" {
		t.Errorf("describeAccount(root) = %q, want %q", got, "root")
	}
	if got := describeAccount(jmsd.AccountOption{Username: "svc"}); got != "svc" {
		t.Errorf("describeAccount(no name) = %q, want %q", got, "svc")
	}
}
