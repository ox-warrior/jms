// Package jmsdclient is jms's client for jmsd's HTTP API (docs/PLAN.md §4
// hop①/hop②). Every request is signed with the user's own SSH private key --
// the same identity already used to log into Koko directly (see
// internal/sshclient.LoadIdentity) -- so there is no separate credential to
// manage. There is no session token: each call carries a fresh timestamp and
// nonce and is verified independently by jmsd.
//
// The caller identifies themself by their public key's SHA256 fingerprint,
// not a username: jmsd resolves the fingerprint to a JumpServer user against
// its cached index of every registered SSH key (internal/jmsd's
// userDirectory), so nothing needs to be typed. Call Whoami once after
// constructing a Client to learn the resolved username for display.
package jmsdclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
)

// MFARequiredError is what every signed call returns when jmsd reports the
// caller's last MFA verification is stale. Callers should prompt for a TOTP
// code, call VerifyMFA, and retry the original request (see
// internal/tui.withMFA).
//
// It carries Username because jmsd resolves the caller's fingerprint to a user
// before it ever checks MFA freshness, so it already knows this even on the
// very first call this process makes (see internal/jmsd.Server.authorize) --
// letting the UI show a real username in the MFA prompt instead of falling
// back to the fingerprint.
type MFARequiredError struct {
	Username string
}

func (e *MFARequiredError) Error() string { return "mfa required" }

// StatusError is any other refusal from jmsd, carrying the HTTP status
// alongside jmsd's own message.
//
// The status is not decoration. Callers that retry have to tell a rejected
// input from a condition no retry can satisfy: the MFA prompt loops on 401
// ("invalid code") but must give up on 429 (locked out for 15 minutes) and on
// 400 ("no MFA device enrolled"), neither of which another code can fix.
type StatusError struct {
	Code    int
	Path    string
	Message string
}

func (e *StatusError) Error() string { return e.Path + ": " + e.Message }

// Client talks to one jmsd instance on behalf of the identity carried by
// signer. It doesn't know its own username until the first successful
// request -- see Whoami.
type Client struct {
	baseURL     string
	fingerprint string
	signer      ssh.Signer
	httpClient  *http.Client
}

// New returns a Client for signer (typically from sshclient.LoadIdentity --
// the same key used to log into Koko). The fingerprint used to identify this
// caller to jmsd is derived from signer's public key right away; it is not a
// secret, so computing it doesn't require jmsd's involvement.
func New(baseURL string, signer ssh.Signer) *Client {
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
		signer:      signer,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
	}
}

// get issues a signed GET against path and decodes the response into a fresh
// T, or returns T's zero value with the error. It is what keeps the endpoint
// wrappers below to one line each.
func get[T any](ctx context.Context, c *Client, path string, query url.Values) (T, error) {
	var out T
	if err := c.getSigned(ctx, path, query, &out); err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

// Identity is what jmsd knows about this caller and about itself, from the one
// /v1/whoami call jms makes at startup.
type Identity struct {
	// Username is the JumpServer user the caller's key fingerprint resolved
	// to. Purely for display -- every other call identifies the caller by
	// fingerprint, not by this.
	Username string `json:"username"`
	// ServerVersion is jmsd's build version, empty when talking to a jmsd
	// predating the field. Callers must treat empty as "unknown", never as a
	// mismatch: an old daemon that cannot answer is not the same as one
	// answering something different.
	ServerVersion string `json:"server_version"`
	// JumpServerBaseline is the JumpServer release jmsd declares it was
	// verified against, for comparison with this client's own declaration.
	// Same empty-means-unknown rule as ServerVersion.
	JumpServerBaseline string `json:"jumpserver_baseline"`
}

// Whoami asks jmsd which JumpServer user this client's SSH key fingerprint
// resolves to, and which version of jmsd is answering (GET /v1/whoami).
func (c *Client) Whoami(ctx context.Context) (Identity, error) {
	return get[Identity](ctx, c, "/v1/whoami", nil)
}

// K8sConfig fetches the office network's front door to Koko's :5002 k8s
// proxy from jmsd (GET /v1/k8s-config) -- configured once in
// config-jmsd.yaml rather than per laptop (docs/K9S_LOCAL_PROXY_PLAN.md). An
// empty ProxyURL means the operator hasn't set this up.
func (c *Client) K8sConfig(ctx context.Context) (jmsd.K8sConfigResponse, error) {
	return get[jmsd.K8sConfigResponse](ctx, c, "/v1/k8s-config", nil)
}

// Nodes fetches the caller's permission node tree from jmsd (GET /v1/nodes).
func (c *Client) Nodes(ctx context.Context) ([]jmsd.NodeTree, error) {
	return get[[]jmsd.NodeTree](ctx, c, "/v1/nodes", nil)
}

// Assets fetches every asset the caller may reach (GET /v1/assets). There is
// no node filter: scope, kind and search are all applied locally to this one
// listing, which is also what the scope tree's per-node counts are computed
// from (internal/tui.ResourceQuery).
func (c *Client) Assets(ctx context.Context) ([]jmsd.AssetRow, error) {
	return get[[]jmsd.AssetRow](ctx, c, "/v1/assets", nil)
}

// Marks fetches the caller's starred assets and recently-connected assets
// from jmsd (GET /v1/marks). Both live in Core, not on this machine, so they
// are shared with the JumpServer web console and follow the user around.
func (c *Client) Marks(ctx context.Context) (jmsd.MarksResponse, error) {
	return get[jmsd.MarksResponse](ctx, c, "/v1/marks", nil)
}

// SetFavorite stars or unstars one asset (POST /v1/favorites). starred is the
// desired end state rather than a toggle, so retrying a request whose
// response was lost cannot flip it back. The signature binds asset|starred
// for the same reason Connect binds asset|account|protocol.
func (c *Client) SetFavorite(ctx context.Context, assetID string, starred bool) error {
	return c.postSigned(ctx, "/v1/favorites",
		[]string{assetID, strconv.FormatBool(starred)},
		map[string]any{"asset": assetID, "starred": starred},
		nil,
	)
}

// VerifyMFA submits a TOTP code to jmsd (POST /v1/auth/mfa). On success,
// jmsd resets the caller's MFA-interval clock, and subsequent Nodes/Assets
// calls stop returning MFARequiredError until it lapses again.
func (c *Client) VerifyMFA(ctx context.Context, code string) error {
	return c.postSigned(ctx, "/v1/auth/mfa", []string{code}, map[string]any{"code": code}, nil)
}

// AssetDetail fetches the accounts and Koko-compatible protocols available
// for one asset (GET /v1/assets/detail).
func (c *Client) AssetDetail(ctx context.Context, assetID string) (jmsd.AssetDetail, error) {
	return get[jmsd.AssetDetail](ctx, c, "/v1/assets/detail", url.Values{"asset": {assetID}})
}

// Connect mints a connection ticket for one asset+account+protocol
// (POST /v1/connections). The signature binds asset|account|protocol, in
// that order, matching jmsd.Server.handleConnections exactly -- like
// VerifyMFA binding code, this stops a signature captured from a read call
// being replayed here against an attacker-chosen asset.
func (c *Client) Connect(ctx context.Context, assetID, account, protocol string) (jmsd.ConnectionTicket, error) {
	var out jmsd.ConnectionTicket
	err := c.postSigned(ctx, "/v1/connections", []string{assetID, account, protocol}, map[string]any{
		"asset": assetID, "account": account, "protocol": protocol,
	}, &out)
	return out, err
}

// postSigned issues a signed POST with a JSON body built from the standard
// fp/ts/nonce/sig plus fields, decoding a 200 response into out (out may be
// nil when the caller only cares whether the call succeeded).
func (c *Client) postSigned(ctx context.Context, path string, extra []string, fields map[string]any, out any) error {
	ts, nonce, sig, err := c.sign(extra...)
	if err != nil {
		return err
	}
	body := map[string]any{"fp": c.fingerprint, "ts": ts, "nonce": nonce, "sig": sig}
	maps.Copy(body, fields)
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	return c.do(req, path, out)
}

// getSigned issues a signed GET request against path with extraQuery merged
// in, and decodes a 200 response into out. A 401 with {"mfa_required":true}
// becomes MFARequiredError.
func (c *Client) getSigned(ctx context.Context, path string, extraQuery url.Values, out any) error {
	ts, nonce, sig, err := c.sign()
	if err != nil {
		return err
	}

	q := url.Values{}
	maps.Copy(q, extraQuery)
	q.Set("fp", c.fingerprint)
	q.Set("ts", ts)
	q.Set("nonce", nonce)
	q.Set("sig", sig)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return c.do(req, path, out)
}

// do sends req and interprets the response the one way both verbs need it
// interpreted: a 401 carrying mfa_required becomes MFARequiredError (with the
// username jmsd resolved the caller's fingerprint to), any other non-200
// becomes jmsd's own error text, and a 200 body is decoded into out. out may
// be nil when only success matters.
func (c *Client) do(req *http.Request, path string, out any) error {
	// Logged here rather than at each of the nine wrappers: this is the one
	// place every jmsd call passes through, so nothing can be added later
	// that escapes the log. The query string is never logged -- it carries
	// the request signature.
	started := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		jmslog.Printf("%s %s: FAILED after %s: %v", req.Method, path,
			time.Since(started).Round(time.Millisecond), err)
		return err
	}
	defer resp.Body.Close()
	// Only the ones that went wrong. Every browse issues several of these, so
	// logging the successes buried everything else.
	elapsed := time.Since(started).Round(time.Millisecond)
	if resp.StatusCode != http.StatusOK {
		jmslog.Printf("%s %s -> %d in %s", req.Method, path, resp.StatusCode, elapsed)
	} else {
		jmslog.Debugf("%s %s -> %d in %s", req.Method, path, resp.StatusCode, elapsed)
	}

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusUnauthorized {
			var body struct {
				MFARequired bool   `json:"mfa_required"`
				Username    string `json:"username"`
			}
			if json.Unmarshal(data, &body) == nil && body.MFARequired {
				return &MFARequiredError{Username: body.Username}
			}
		}
		return &StatusError{Code: resp.StatusCode, Path: path, Message: errorMessage(data)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// sign builds a fresh timestamp+nonce and signs
// "fingerprint|ts|nonce[|extra...]" with c.signer, matching
// internal/jmsd.Server.verifySignedRequest's payload construction exactly.
func (c *Client) sign(extra ...string) (ts, nonce, sig string, err error) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	nonce, err = randomNonce()
	if err != nil {
		return "", "", "", err
	}
	payload := strings.Join(append([]string{c.fingerprint, ts, nonce}, extra...), "|")

	signature, err := c.signer.Sign(rand.Reader, []byte(payload))
	if err != nil {
		return "", "", "", fmt.Errorf("sign request: %w", err)
	}
	sig = base64.StdEncoding.EncodeToString(ssh.Marshal(signature))
	return ts, nonce, sig, nil
}

func randomNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// errorMessage pulls jmsd's own {"error": "..."} text out of a refusal,
// falling back to the raw body for anything that isn't one (a reverse proxy's
// own error page, say).
func errorMessage(data []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && body.Error != "" {
		return body.Error
	}
	return string(data)
}
