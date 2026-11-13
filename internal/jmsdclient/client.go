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
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/jmsd"
	"github.com/leiyupeng/jumpserver-cli/internal/jmslog"
)

// MFARequiredError is what every signed call returns when jmsd reports the
// caller's key is not signed in -- never was, or the binding lapsed. Callers
// sign in with Google (VerifyGoogle) and retry the original request (see
// connectflow.WithMFA). The name, and the "mfa_required" it is decoded from,
// predate Google sign-in; renaming the wire field would break every deployed
// jms at once.
type MFARequiredError struct{}

func (e *MFARequiredError) Error() string { return "mfa required" }

// StatusError is any other refusal from jmsd, carrying the HTTP status
// alongside jmsd's own message.
//
// The status is kept so a caller can tell jmsd refusing the request (4xx)
// from jmsd failing to serve it (5xx).
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
	// OnBehalfOf names the person a service key is acting for (the gateway
	// that holds the key knows who asked; jmsd cannot). Signed into every
	// request so it cannot be swapped in transit, and recorded in jmsd's
	// audit. jmsd refuses it from anything but a service key.
	OnBehalfOf string
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

// TunnelConfig fetches where jmsd's tunnel listens and which host key it will
// present (GET /v1/tunnel-config). An empty Addr means the operator has not
// enabled the tunnel.
//
// This call is also what signs the tunnel's key in: it runs through jmsd's
// ordinary authorize(), so an unknown or lapsed key comes back as
// MFARequiredError here -- on the control plane, where connectflow.WithMFA
// opens the Google sign-in -- rather than as an opaque SSH authentication
// failure later.
func (c *Client) TunnelConfig(ctx context.Context) (jmsd.TunnelConfigResponse, error) {
	return get[jmsd.TunnelConfigResponse](ctx, c, "/v1/tunnel-config", nil)
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

// VerifyGoogle submits a Google ID token to sign this key in (POST
// /v1/auth/google). nonce must be the one the Google sign-in was started with:
// jmsd requires the token to carry this request's nonce, which is what stops
// a token from being replayed or moved to another request.
//
// The public key travels with it because jmsd may never have seen this key:
// the sign-in is what binds it to a JumpServer user.
func (c *Client) VerifyGoogle(ctx context.Context, nonce, idToken string) error {
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(c.signer.PublicKey())))
	return c.postSignedWithNonce(ctx, "/v1/auth/google", nonce, []string{pub, idToken},
		map[string]any{"public_key": pub, "id_token": idToken}, nil)
}

// AssetDetail fetches the accounts and Koko-compatible protocols available
// for one asset (GET /v1/assets/detail).
func (c *Client) AssetDetail(ctx context.Context, assetID string) (jmsd.AssetDetail, error) {
	return get[jmsd.AssetDetail](ctx, c, "/v1/assets/detail", url.Values{"asset": {assetID}})
}

// Describe fetches one machine's cloud metadata (GET /v1/assets/describe).
// key comes from AssetRow.CloudKey; an empty key has no metadata and must not
// be sent.
func (c *Client) Describe(ctx context.Context, key string) (jmsd.HostInfo, error) {
	return get[jmsd.HostInfo](ctx, c, "/v1/assets/describe", url.Values{"key": {key}})
}

// Connect mints a connection ticket for one asset+account+protocol
// (POST /v1/connections). The signature binds asset|account|protocol, in
// that order, matching jmsd.Server.handleConnections exactly -- this stops a signature captured from a read call
// being replayed here against an attacker-chosen asset.
//
// command is the remote command the ticket is minted for ("" for an
// interactive session). It goes to jmsd's audit, signed in with the rest so
// it cannot be swapped in transit -- though a ticket is not bound to it, so
// this is a record of what jms said it would run, not a restriction.
func (c *Client) Connect(ctx context.Context, assetID, account, protocol, command string) (jmsd.ConnectionTicket, error) {
	extra := []string{assetID, account, protocol}
	fields := map[string]any{"asset": assetID, "account": account, "protocol": protocol}
	if command != "" {
		extra = append(extra, command)
		fields["command"] = command
	}
	var out jmsd.ConnectionTicket
	err := c.postSigned(ctx, "/v1/connections", extra, fields, &out)
	return out, err
}

// Query runs one read-only SQL statement through jmsd (POST /v1/query). A
// database-side failure comes back as QueryResult.Error, not as err.
//
// Its own HTTP timeout: jmsd lets a query run 30s, past the 15s every other
// call is held to.
func (c *Client) Query(ctx context.Context, assetID, account, protocol, sql string) (jmsd.QueryResult, error) {
	slow := *c
	slow.httpClient = &http.Client{Timeout: 45 * time.Second}
	var out jmsd.QueryResult
	err := slow.postSigned(ctx, "/v1/query", []string{assetID, account, protocol, sql}, map[string]any{
		"asset": assetID, "account": account, "protocol": protocol, "sql": sql,
	}, &out)
	return out, err
}

// postSigned issues a signed POST with a JSON body built from the standard
// fp/ts/nonce/sig plus fields, decoding a 200 response into out (out may be
// nil when the caller only cares whether the call succeeded).
func (c *Client) postSigned(ctx context.Context, path string, extra []string, fields map[string]any, out any) error {
	nonce, err := NewNonce()
	if err != nil {
		return err
	}
	return c.postSignedWithNonce(ctx, path, nonce, extra, fields, out)
}

// postSignedWithNonce is postSigned for the one caller that has to fix the
// nonce in advance: the Google sign-in, which embeds it in the ID token.
func (c *Client) postSignedWithNonce(ctx context.Context, path, nonce string, extra []string, fields map[string]any, out any) error {
	ts, sig, err := c.signWith(nonce, extra...)
	if err != nil {
		return err
	}
	body := map[string]any{"fp": c.fingerprint, "ts": ts, "nonce": nonce, "sig": sig}
	if c.OnBehalfOf != "" {
		body["on_behalf_of"] = c.OnBehalfOf
	}
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
	if c.OnBehalfOf != "" {
		q.Set("on_behalf_of", c.OnBehalfOf)
	}

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
		return transportError(c.baseURL, path, c.httpClient.Timeout, err)
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
				MFARequired bool `json:"mfa_required"`
			}
			if json.Unmarshal(data, &body) == nil && body.MFARequired {
				return &MFARequiredError{}
			}
		}
		return &StatusError{Code: resp.StatusCode, Path: path, Message: errorMessage(data)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// transportError turns a net/http failure into something worth reading.
//
// The error http.Client returns embeds the full request URL, and these URLs
// carry a signature roughly a thousand characters long in the query string.
// Printed as-is it buried the actual reason -- a timeout, a refused connection
// -- past a screenful of base64, and the caller wrapping it as "resolve
// identity" then pointed at the wrong thing entirely: nothing was wrong with
// the identity, jmsd was simply unreachable.
//
// The address is kept, since a wrong jmsd_addr looks exactly like an outage and
// is the first thing worth checking. The query string is dropped for the same
// reason the log line above omits it.
func transportError(baseURL, path string, timeout time.Duration, err error) error {
	// url.Error's own message is the part carrying the signature; its wrapped
	// Err is the part that explains the failure.
	reason := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		reason = urlErr.Err.Error()
		if urlErr.Timeout() {
			return fmt.Errorf("jmsd at %s did not answer within %s: %s",
				baseURL, timeout, reason)
		}
	}
	return fmt.Errorf("cannot reach jmsd at %s%s: %s", baseURL, path, reason)
}

// sign builds a fresh timestamp+nonce and signs
// "fingerprint|ts|nonce[|extra...]" with c.signer, matching
// internal/jmsd.Server.verifySignedRequest's payload construction exactly.
func (c *Client) sign(extra ...string) (ts, nonce, sig string, err error) {
	nonce, err = NewNonce()
	if err != nil {
		return "", "", "", err
	}
	ts, sig, err = c.signWith(nonce, extra...)
	return ts, nonce, sig, err
}

func (c *Client) signWith(nonce string, extra ...string) (ts, sig string, err error) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	if c.OnBehalfOf != "" {
		// Last, and only when set: requests without it sign exactly what
		// they always did, so older jmsd and jms keep agreeing.
		extra = append(slices.Clip(extra), jmsd.OnBehalfPrefix+c.OnBehalfOf)
	}
	payload := strings.Join(append([]string{c.fingerprint, ts, nonce}, extra...), "|")
	signature, err := c.signer.Sign(rand.Reader, []byte(payload))
	if err != nil {
		return "", "", fmt.Errorf("sign request: %w", err)
	}
	return ts, base64.StdEncoding.EncodeToString(ssh.Marshal(signature)), nil
}

// NewNonce returns a fresh request nonce. Exported for the Google sign-in,
// which has to know the nonce before the request exists (see VerifyGoogle).
func NewNonce() (string, error) {
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
