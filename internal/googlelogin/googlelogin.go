// Package googlelogin runs Google's OAuth sign-in in the user's browser and
// returns the ID token, the way gcloud or Claude Code's MCP login does: a
// listener on 127.0.0.1 catches the redirect, and PKCE makes the code it
// receives useless to anyone else.
//
// It only obtains the token. Deciding what the token is worth -- right
// client, company account, same person as the SSH key -- is jmsd's job
// (internal/jmsd/googleauth.go); jms never trusts its own copy.
package googlelogin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	authURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenURL = "https://oauth2.googleapis.com/token"
)

// Timeout bounds how long SignIn waits for the browser. Long enough to find
// the tab and pick an account; short enough that a forgotten sign-in does not
// keep a port open all afternoon.
const Timeout = 5 * time.Minute

// Client is a Google OAuth client of the "Desktop app" type. Secret is not a
// secret in any real sense -- Google ships it inside desktop apps by design --
// and PKCE is what actually protects the exchange.
type Client struct {
	ID     string
	Secret string
}

// SignIn opens the browser on Google's consent page and waits for the user to
// finish. nonce is embedded in the ID token; jmsd checks it against the
// request the token is submitted with. show receives the URL so a caller can
// print it for when no browser opens (an SSH session, a headless box).
func (c Client) SignIn(ctx context.Context, nonce string, show func(url string)) (idToken string, err error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	// 127.0.0.1 and not "localhost": the redirect must reach this listener,
	// and localhost can resolve to ::1 first.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen for the sign-in redirect: %w", err)
	}
	redirectURI := "http://" + ln.Addr().String()

	verifier, err := randomString(32)
	if err != nil {
		return "", err
	}
	state, err := randomString(16)
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	consent := authURL + "?" + url.Values{
		"client_id":             {c.ID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"openid email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		// Always show the account chooser: a browser signed in to a personal
		// account would otherwise pick it silently, and jmsd would refuse it
		// with nothing on screen to explain why.
		"prompt": {"select_account"},
	}.Encode()

	codes := make(chan string, 1)
	failures := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// state is checked so that a page the user happens to have open
		// cannot feed this listener a code of its own choosing.
		if q.Get("state") != state {
			http.Error(w, "unexpected sign-in response", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if e := q.Get("error"); e != "" {
			fmt.Fprint(w, page("登录没有完成，可以关掉这个页面，回到终端看原因。"))
			select {
			case failures <- fmt.Errorf("Google sign-in refused: %s", e):
			default:
			}
			return
		}
		fmt.Fprint(w, page("登录成功，可以关掉这个页面了。"))
		select {
		case codes <- q.Get("code"):
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	show(consent)
	openBrowser(consent)

	var code string
	select {
	case code = <-codes:
	case err := <-failures:
		return "", err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("Google sign-in not finished within %s", Timeout)
		}
		return "", ctx.Err()
	}
	return c.exchange(ctx, code, verifier, redirectURI)
}

// exchange trades the authorization code for tokens and keeps only the ID
// token. The access token is never used, so it is not kept either.
func (c Client) exchange(ctx context.Context, code, verifier, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {c.ID},
		"client_secret": {c.Secret},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange Google sign-in code: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		IDToken     string `json:"id_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("exchange Google sign-in code: %s", resp.Status)
	}
	if out.IDToken == "" {
		return "", fmt.Errorf("exchange Google sign-in code: %s %s", out.Error, out.Description)
	}
	return out.IDToken, nil
}

// openBrowser is best effort: when it fails the URL has already been shown,
// and that is the fallback.
func openBrowser(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func page(msg string) string {
	return `<!doctype html><meta charset="utf-8"><title>jms</title>` +
		`<body style="font-family:sans-serif;padding:3em"><p>` + msg + `</p></body>`
}
