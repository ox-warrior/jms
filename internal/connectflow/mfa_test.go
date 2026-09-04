package connectflow

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsdclient"
)

func TestMFACodeRejected(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		retry bool
	}{
		{"wrong code", &jmsdclient.StatusError{Code: http.StatusUnauthorized, Path: "/v1/auth/mfa", Message: "invalid code"}, true},
		{"locked out", &jmsdclient.StatusError{Code: http.StatusTooManyRequests, Path: "/v1/auth/mfa", Message: "too many incorrect MFA codes"}, false},
		{"no device enrolled", &jmsdclient.StatusError{Code: http.StatusBadRequest, Path: "/v1/auth/mfa", Message: "no MFA device enrolled"}, false},
		{"state store down", &jmsdclient.StatusError{Code: http.StatusServiceUnavailable, Path: "/v1/auth/mfa", Message: "auth state store unavailable"}, false},
		{"never reached jmsd", errors.New("dial tcp: connection refused"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MFACodeRejected(c.err); got != c.retry {
				t.Errorf("MFACodeRejected(%v) = %t, want %t", c.err, got, c.retry)
			}
		})
	}
}

// mfaServer is a minimal stand-in for jmsd's POST /v1/auth/mfa, real enough
// that jmsdclient.Client's actual HTTP/signing path runs against it -- so
// HandleMFA is exercised as shipped, not as a re-description of itself.
// verify decides each submitted code's outcome; it does not check the
// signature jmsdclient attaches, since that is jmsdclient's own concern
// (internal/jmsdclient has no tests of its own yet, but its signing logic is
// independent of what HandleMFA does with the response).
func mfaServer(t *testing.T, verify func(code string) (status int, body string)) *jmsdclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/mfa", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		status, body := verify(req.Code)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("build test signer: %v", err)
	}
	return jmsdclient.New(server.URL, signer)
}

func TestHandleMFARetriesOnlyOnRejectedCode(t *testing.T) {
	t.Run("wrong code then right code succeeds", func(t *testing.T) {
		jc := mfaServer(t, func(code string) (int, string) {
			if code == "222222" {
				return http.StatusOK, "{}"
			}
			return http.StatusUnauthorized, `{"error":"invalid code"}`
		})
		attempts := []string{"111111", "222222"}
		i := 0
		prompt := func(_ context.Context, _, errMsg string) (string, error) {
			if i == 0 && errMsg != "" {
				t.Errorf("first prompt got a non-empty errMsg %q", errMsg)
			}
			if i == 1 && errMsg == "" {
				t.Error("second prompt (after a rejected code) got an empty errMsg")
			}
			code := attempts[i]
			i++
			return code, nil
		}
		if err := HandleMFA(context.Background(), jc, "alice", prompt); err != nil {
			t.Fatalf("HandleMFA() = %v, want nil", err)
		}
		if i != 2 {
			t.Errorf("prompt called %d times, want 2", i)
		}
	})

	t.Run("locked out stops retrying immediately", func(t *testing.T) {
		jc := mfaServer(t, func(string) (int, string) {
			return http.StatusTooManyRequests, `{"error":"too many incorrect MFA codes; try again in 14m52s"}`
		})
		calls := 0
		prompt := func(context.Context, string, string) (string, error) {
			calls++
			return "111111", nil
		}
		err := HandleMFA(context.Background(), jc, "alice", prompt)
		var status *jmsdclient.StatusError
		if !errors.As(err, &status) || status.Code != http.StatusTooManyRequests {
			t.Errorf("HandleMFA() = %v, want the 429 returned unwrapped", err)
		}
		if calls != 1 {
			t.Errorf("prompt called %d times, want exactly 1 -- a lockout must not reopen the prompt", calls)
		}
	})

	t.Run("cancelling the prompt returns ErrCancelled", func(t *testing.T) {
		jc := mfaServer(t, func(string) (int, string) {
			return http.StatusUnauthorized, `{"error":"invalid code"}`
		})
		prompt := func(context.Context, string, string) (string, error) {
			return "", errors.New("user pressed Esc")
		}
		if err := HandleMFA(context.Background(), jc, "alice", prompt); !errors.Is(err, ErrCancelled) {
			t.Errorf("HandleMFA() = %v, want ErrCancelled", err)
		}
	})
}

func TestWithMFAPromptsOnceThenRetriesTheCallExactlyOnce(t *testing.T) {
	jc := mfaServer(t, func(code string) (int, string) {
		if code == "111111" {
			return http.StatusOK, "{}"
		}
		return http.StatusUnauthorized, `{"error":"invalid code"}`
	})

	promptCalls := 0
	gate := &Gate{
		JC:  jc,
		Ctx: context.Background(),
		Prompt: func(context.Context, string, string) (string, error) {
			promptCalls++
			return "111111", nil
		},
	}

	callCalls := 0
	out, err := WithMFA(gate, func() (string, error) {
		callCalls++
		if callCalls == 1 {
			return "", &jmsdclient.MFARequiredError{Username: "alice"}
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("WithMFA() error = %v, want nil", err)
	}
	if out != "ok" {
		t.Errorf("WithMFA() = %q, want %q", out, "ok")
	}
	if callCalls != 2 {
		t.Errorf("call invoked %d times, want 2 (once lapsed, once after MFA)", callCalls)
	}
	if promptCalls != 1 {
		t.Errorf("prompt invoked %d times, want 1", promptCalls)
	}
}

func TestWithMFAPassesThroughAnyOtherError(t *testing.T) {
	gate := &Gate{Ctx: context.Background()}
	wantErr := errors.New("load failed")
	_, err := WithMFA(gate, func() (string, error) { return "", wantErr })
	if !errors.Is(err, wantErr) {
		t.Errorf("WithMFA() = %v, want %v unwrapped -- only MFARequiredError should trigger a prompt", err, wantErr)
	}
}

func TestGateAroundWrapsThePrompt(t *testing.T) {
	jc := mfaServer(t, func(string) (int, string) { return http.StatusOK, "{}" })

	var aroundCalled, promptCalled bool
	gate := &Gate{
		JC:  jc,
		Ctx: context.Background(),
		Prompt: func(context.Context, string, string) (string, error) {
			promptCalled = true
			return "111111", nil
		},
		Around: func(ask func() error) error {
			aroundCalled = true
			return ask()
		},
	}
	_, err := WithMFA(gate, func() (string, error) {
		return "", &jmsdclient.MFARequiredError{Username: "alice"}
	})
	// The stub call always reports MFARequiredError, so a second call after a
	// successful prompt reports it again -- what matters here is only that
	// Around and Prompt both ran, not the final error.
	if !aroundCalled {
		t.Error("Gate.Around was never called")
	}
	if !promptCalled {
		t.Error("Gate.Prompt was never called")
	}
	_ = err
}

func TestWithMFAUsesMFAErrorUsernameWhenGateHasNone(t *testing.T) {
	jc := mfaServer(t, func(string) (int, string) { return http.StatusOK, "{}" })

	var gotUsername string
	gate := &Gate{
		JC:  jc,
		Ctx: context.Background(),
		Prompt: func(_ context.Context, username, _ string) (string, error) {
			gotUsername = username
			return "111111", nil
		},
	}
	_, _ = WithMFA(gate, func() (string, error) {
		return "", &jmsdclient.MFARequiredError{Username: "bob"}
	})
	if gotUsername != "bob" {
		t.Errorf("prompt username = %q, want %q -- jmsd resolves the fingerprint before checking MFA, so even the first call names the caller", gotUsername, "bob")
	}
}
