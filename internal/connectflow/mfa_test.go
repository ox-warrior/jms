package connectflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
)

// authGate returns a Gate whose sign-in only records the username it was
// given. authDone is reset because the process-wide "signed in a moment ago"
// shortcut would otherwise skip every sign-in after the first test's.
func authGate(t *testing.T, got *[]string) *Gate {
	t.Helper()
	authDone = time.Time{}
	t.Cleanup(func() { authDone = time.Time{} })
	return &Gate{Ctx: context.Background(), Authenticate: func(_ context.Context, username string) error {
		*got = append(*got, username)
		return nil
	}}
}

func TestWithMFASignsInOnceThenRetriesTheCallExactlyOnce(t *testing.T) {
	var signIns []string
	gate := authGate(t, &signIns)
	calls := 0
	out, err := WithMFA(gate, func() (string, error) {
		calls++
		if calls == 1 {
			return "", &jmsdclient.MFARequiredError{}
		}
		return "ok", nil
	})
	if err != nil || out != "ok" {
		t.Fatalf("WithMFA() = %q, %v; want ok", out, err)
	}
	if calls != 2 || len(signIns) != 1 {
		t.Errorf("call ran %d times, sign-in %d times; want 2 and 1", calls, len(signIns))
	}
}

func TestWithMFAPassesThroughAnyOtherError(t *testing.T) {
	gate := &Gate{Ctx: context.Background()}
	wantErr := errors.New("load failed")
	_, err := WithMFA(gate, func() (string, error) { return "", wantErr })
	if !errors.Is(err, wantErr) {
		t.Errorf("WithMFA() = %v, want %v unwrapped -- only MFARequiredError should trigger a sign-in", err, wantErr)
	}
}

func TestGateAroundWrapsTheSignIn(t *testing.T) {
	var signIns []string
	gate := authGate(t, &signIns)
	aroundCalled := false
	gate.Around = func(ask func() error) error {
		aroundCalled = true
		return ask()
	}
	_, _ = WithMFA(gate, func() (string, error) {
		return "", &jmsdclient.MFARequiredError{}
	})
	if !aroundCalled || len(signIns) != 1 {
		t.Errorf("Around called %t, sign-in %d times; want both", aroundCalled, len(signIns))
	}
}
