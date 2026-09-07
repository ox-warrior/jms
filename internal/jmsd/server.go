package jmsd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jumpserver-dev/sdk-go/service"
	"golang.org/x/crypto/ssh"

	"github.com/leiyupeng/jumpserver-cli/internal/version"
)

// Server is jmsd's HTTP API. It holds a Core client authenticated as its own
// JumpServer account (docs/PLAN.md §7.1) for resource queries, and a
// read-only connection to Core's database (§7.2) to verify a caller's SSH
// signature and TOTP code. jmsd never touches target-asset secrets: the
// perms endpoints it calls don't return them, and it never asks Core for a
// connection token's secret.
type Server struct {
	cfg    Config
	core   *service.JMService
	coreDB *CoreDB
	// inventory is the cloud-metadata database, nil when inventory_db_dsn is
	// unset. Only handleAssetDescribe touches it, and it checks for nil --
	// this is a display feature and must never be able to break a connection.
	inventory *InventoryDB
	// users resolves a caller's key fingerprint to a JumpServer user from a
	// cached index, so an unauthenticated caller cannot drive Core's database
	// by sending forged signatures -- see userDirectory.
	users *userDirectory
	// mfaFailures caps how many wrong TOTP codes one user may submit before
	// the endpoint stops answering -- see failureTracker.
	mfaFailures *failureTracker
	state       StateStore
	// stateDesc names the state backend for the log, so "mfa verified" says
	// where it was persisted instead of leaving that to be checked by hand.
	stateDesc string
}

// AssetRow is one row of a resource listing (GET /v1/assets). It
// intentionally omits protocol/account: those come from the per-asset detail
// call once the user selects a row, so listing N assets costs one Core
// request regardless of N.
type AssetRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Category string `json:"category"`
	Type     string `json:"type"`
	// NodeIDs is the asset's direct node membership, used by the client to
	// tally per-node match counts without asking Core per node. It lists only
	// direct parents, never ancestors -- see permAsset in permassets.go.
	NodeIDs []string `json:"node_ids"`
	// CloudKey addresses this asset's row in the cloud-metadata table, and is
	// what GET /v1/assets/describe takes. Empty means there is nothing to
	// show -- either the asset is not cloud-sync managed, or no inventory
	// database is configured -- which lets the client answer "d" without a
	// round-trip when the answer is "nothing".
	//
	// Only a key travels, not the whole row: the rows are a few hundred bytes
	// each and most are never looked at, so attaching them all would spend
	// hundreds of kilobytes per listing on a panel opened a few times a day.
	CloudKey string `json:"cloud_key,omitempty"`
	// InstanceType is the machine's shape, shown as a column in the client's
	// resource table. Only hosts have one.
	//
	// It is here rather than behind CloudKey because it is worth seeing while
	// scanning a list, which is the whole bar for adding a field to this
	// struct -- see jmsd.cloudBrief.
	InstanceType string `json:"instance_type,omitempty"`
	// OwnerTeam and AppModule are the other two listing-visible cloud fields,
	// shown as columns beside InstanceType. Same bar as above: short, single
	// value, worth seeing while scanning.
	OwnerTeam string `json:"owner_team,omitempty"`
	AppModule string `json:"app_module,omitempty"`
}

// authedHandler is an HTTP handler that only runs after requireSignature has
// proved who the caller is. user is the record read from Core's database, so
// handlers never re-resolve the username.
type authedHandler func(w http.ResponseWriter, r *http.Request, user *userRecord)

// debugEnabled gates debugf. A package-level switch rather than a field on
// Server because CoreDB's own query methods need it too and threading a
// logger through them would be noise -- there is exactly one Server per
// process, set once in New before any request is served.
var debugEnabled bool

// debugf logs a line only under --debug. For anything that happens several
// times per request: database queries, Core API calls, pagination.
func debugf(format string, args ...any) {
	if debugEnabled {
		log.Printf("jmsd: debug: "+format, args...)
	}
}

// timed reports how long something took, at debug level. Use as
//
//	defer timed("query recent assets for %s", userID)()
//
// It is the answer to "which of the four things this request does is slow" --
// jmsd fans one client request out into a fingerprint scan, a Core API call
// and sometimes a paginated crawl, and from outside they are one duration.
func timed(format string, args ...any) func() {
	if !debugEnabled {
		return func() {}
	}
	what := fmt.Sprintf(format, args...)
	started := time.Now()
	return func() { debugf("%s took %s", what, time.Since(started).Round(time.Millisecond)) }
}

func New(cfg Config) (*Server, error) {
	debugEnabled = cfg.DebugEnabled()
	core, err := service.NewAuthJMService(
		service.JMSCoreHost(cfg.CoreHost),
		service.JMSAccessKey(cfg.AccessKeyID, cfg.AccessKeySecret),
	)
	if err != nil {
		return nil, err
	}
	coreDB, err := NewCoreDB(cfg.CoreDBDSN, cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	// No redis block configured keeps the original in-memory behaviour, so
	// existing deployments need no config change -- they just keep losing MFA
	// state on restart. Which store won is logged, and named again on every
	// "mfa verified" line, because the two are otherwise indistinguishable
	// from outside: an omitted redis block looks exactly like a working one
	// until someone restarts jmsd and finds themselves retyping a TOTP code
	// with no way to tell whether the feature is off or broken.
	var state StateStore = newMemStateStore()
	stateDesc := "process memory"
	if cfg.Redis.Addr != "" {
		state, err = newRedisStateStore(cfg.Redis, time.Duration(cfg.MFAIntervalDays)*24*time.Hour)
		if err != nil {
			_ = coreDB.Close()
			return nil, err
		}
		stateDesc = fmt.Sprintf("redis %s db %d", cfg.Redis.Addr, cfg.Redis.DB)
		log.Printf("jmsd: auth state in %s, MFA survives restarts", stateDesc)
	} else {
		log.Printf("jmsd: auth state in %s, every restart re-prompts MFA (set redis.addr to persist it)", stateDesc)
	}
	// Opened eagerly, like coreDB, so a wrong DSN surfaces at startup rather
	// than the first time somebody presses "d". Fatal on purpose: the admin
	// asked for this by setting the key, and a daemon that silently drops a
	// configured feature is worse than one that refuses to start.
	var inv *InventoryDB
	if cfg.InventoryDBDSN != "" {
		if inv, err = NewInventoryDB(cfg.InventoryDBDSN); err != nil {
			_ = coreDB.Close()
			_ = state.Close()
			return nil, err
		}
		// Loaded here, not on the first request. The table lives in another
		// region; paying seconds for it at startup costs nobody anything,
		// while paying it on demand would land on whoever opens jms first --
		// which, with a TTL shorter than the gap between launches, is very
		// nearly everybody.
		if err := inv.Warm(); err != nil {
			log.Printf("jmsd: cloud metadata configured but the first load failed, retrying in the background: %v", err)
		} else {
			log.Printf("jmsd: cloud metadata loaded, assets carry it for the \"d\" panel")
		}
	} else {
		log.Printf("jmsd: no inventory_db_dsn, the \"d\" panel will report no cloud metadata")
	}
	return &Server{
		cfg:         cfg,
		core:        core,
		coreDB:      coreDB,
		inventory:   inv,
		users:       newUserDirectory(coreDB.activeSSHKeys, keyIndexTTL),
		mfaFailures: newFailureTracker(mfaMaxFailures, mfaLockout),
		state:       state,
		stateDesc:   stateDesc,
	}, nil
}

// Close releases the database connection and the state store. The Core HTTP
// client holds no closeable resources. Both are attempted even if the first
// fails, since leaving one open helps nobody.
func (s *Server) Close() error {
	err := errors.Join(s.coreDB.Close(), s.state.Close())
	if s.inventory != nil {
		err = errors.Join(err, s.inventory.Close())
	}
	return err
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/whoami", s.requireSignature(s.handleWhoami))
	mux.HandleFunc("/v1/k8s-config", s.requireSignature(s.handleK8sConfig))
	mux.HandleFunc("/v1/nodes", s.requireSignature(s.handleNodes))
	mux.HandleFunc("/v1/assets", s.requireSignature(s.handleAssets))
	mux.HandleFunc("/v1/assets/detail", s.requireSignature(s.handleAssetDetail))
	mux.HandleFunc("/v1/assets/describe", s.requireSignature(s.handleDescribe))
	mux.HandleFunc("/v1/marks", s.requireSignature(s.handleMarks))
	mux.HandleFunc("/v1/auth/mfa", s.handleMFA)
	mux.HandleFunc("/v1/favorites", s.handleSetFavorite)
	mux.HandleFunc("/v1/connections", s.handleConnections)
	return logging(mux)
}

// MarksResponse is the body of GET /v1/marks: the caller's starred assets and
// the assets they most recently connected to. Both come from Core's own
// tables (see CoreDB.Favorites / CoreDB.RecentAssets), so they are shared
// with the JumpServer web console and follow the user between machines
// instead of living in a file on one laptop.
type MarksResponse struct {
	Favorites []string `json:"favorites"`
	Recent    []string `json:"recent"`
}

func (s *Server) handleMarks(w http.ResponseWriter, r *http.Request, user *userRecord) {
	favorites, err := s.timedFavorites(user)
	if err != nil {
		fail(w, r, user.Username, http.StatusBadGateway, "load favorites: "+err.Error())
		return
	}
	recent, err := s.timedRecent(user)
	if err != nil {
		fail(w, r, user.Username, http.StatusBadGateway, "load recent: "+err.Error())
		return
	}
	writeJSON(w, MarksResponse{Favorites: favorites, Recent: recent})
}

// timedFavorites / timedRecent exist only so the two reads behind /v1/marks
// can be timed separately -- they hit different tables with very different
// cost, and as one line they were indistinguishable.
func (s *Server) timedFavorites(user *userRecord) ([]string, error) {
	defer timed("db read: favorites user=%s", user.ID)()
	return s.coreDB.Favorites(user.ID)
}

func (s *Server) timedRecent(user *userRecord) ([]string, error) {
	defer timed("db read: recent sessions user=%s", user.ID)()
	return s.coreDB.RecentAssets(user.ID, RecentLimit)
}

// favoriteRequest is the body of POST /v1/favorites.
type favoriteRequest struct {
	signedFields
	Asset string `json:"asset"`
	// Starred is the desired end state, not a toggle: making the request
	// idempotent means a retry after a timeout can't silently undo itself.
	Starred bool `json:"starred"`
}

// handleSetFavorite stars or unstars one asset for the calling user. Like
// /v1/connections and /v1/auth/mfa it bypasses the requireSignature
// middleware, because the signature has to cover the asset and the desired
// state -- otherwise a signature captured from a read call could be replayed
// to star or unstar an attacker-chosen asset.
//
// The user is always the hop①-verified caller; there is no way to set another
// user's favorites through this endpoint.
func (s *Server) handleSetFavorite(w http.ResponseWriter, r *http.Request) {
	var req favoriteRequest
	if !decodeSignedPost(w, r, &req) {
		return
	}
	if req.Asset == "" {
		fail(w, r, "", http.StatusBadRequest, "asset is required")
		return
	}
	user := s.authorize(w, r, req.signedFields, req.Asset, strconv.FormatBool(req.Starred))
	if user == nil {
		return
	}

	// One of the only two writes jmsd ever makes to Core's database (the other
	// is the matching delete), so it is logged unconditionally below -- see the
	// note call after this block.
	//
	// Wrapped in a closure so the timer stops when the write does. A bare
	// `defer timed(...)()` here would run at the END of the handler and quietly
	// bill the response encoding to the database.
	err := func() error {
		if req.Starred {
			defer timed("db write: insert favorite asset=%s user=%s", req.Asset, user.ID)()
			return s.coreDB.AddFavorite(user.ID, req.Asset)
		}
		defer timed("db write: delete favorite asset=%s user=%s", req.Asset, user.ID)()
		return s.coreDB.RemoveFavorite(user.ID, req.Asset)
	}()
	if err != nil {
		fail(w, r, user.Username, http.StatusBadGateway, err.Error())
		return
	}
	note(r, user.Username, fmt.Sprintf("favorite asset=%s starred=%v", req.Asset, req.Starred))
	writeJSON(w, map[string]string{"status": "ok"})
}

// signedFields are the four parameters that prove who sent a request
// (docs/PLAN.md §4 hop①). GET endpoints carry them in the query string; the
// POST bodies below embed this struct so they share one definition and one
// completeness check.
type signedFields struct {
	Fingerprint string `json:"fp"`
	Ts          string `json:"ts"`
	Nonce       string `json:"nonce"`
	Sig         string `json:"sig"`
}

func (f signedFields) complete() bool {
	return f.Fingerprint != "" && f.Ts != "" && f.Nonce != "" && f.Sig != ""
}

// signedBody is satisfied by any POST body embedding signedFields, letting
// decodeSignedPost reach the signature without knowing the concrete type.
type signedBody interface{ signature() signedFields }

func (f signedFields) signature() signedFields { return f }

// authorize is the gate every endpoint passes through: it verifies the
// signature over "fingerprint|ts|nonce" plus extra (in order), then checks
// that the caller's last MFA verification is still within
// cfg.MFAIntervalDays. On failure it answers the client itself and returns
// nil, which the caller must treat as "stop here".
//
// extra is what separates a read from a write. Reads sign nothing beyond the
// three base fields; every POST binds its own arguments in, so a signature
// captured from a read cannot be replayed to mint a token, star an asset, or
// submit a TOTP code of the attacker's choosing.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, f signedFields, extra ...string) *userRecord {
	user, err := s.verifySignedRequest(f.Fingerprint, f.Ts, f.Nonce, f.Sig, extra...)
	if err != nil {
		fail(w, r, "", authFailureStatus(err), err.Error())
		return nil
	}
	// Recorded as soon as the signature checks out, so every line logging
	// prints for an authenticated request names the caller -- handlers past
	// this point never have to repeat it.
	note(r, user.Username, "")
	fresh, err := s.mfaFresh(user.Username)
	if err != nil {
		fail(w, r, user.Username, http.StatusServiceUnavailable, err.Error())
		return nil
	}
	if !fresh {
		note(r, user.Username, "mfa required")
		// Username is included here (not just mfa_required) so jms can show it
		// in the MFA prompt even on the very first request, before it has ever
		// received a successful response of any kind -- resolving the
		// fingerprint to a user doesn't require a fresh MFA check, so jmsd
		// already knows it at this point.
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"mfa_required": true, "username": user.Username})
		return nil
	}
	return user
}

// decodeSignedPost is the preamble the three POST endpoints share: enforce
// the method, decode the body into req, and confirm the signature fields are
// all present. It answers the client and returns false on any failure. Each
// caller still checks its own required arguments afterwards, since what is
// missing is worth naming.
func decodeSignedPost(w http.ResponseWriter, r *http.Request, req signedBody) bool {
	if r.Method != http.MethodPost {
		fail(w, r, "", http.StatusMethodNotAllowed, "POST required")
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		fail(w, r, "", http.StatusBadRequest, "invalid request body")
		return false
	}
	if !req.signature().complete() {
		fail(w, r, "", http.StatusBadRequest, "fp, ts, nonce, sig are required")
		return false
	}
	return true
}

// requireSignature is hop① (docs/PLAN.md §4) for the read endpoints: the
// caller signs "fingerprint|ts|nonce" with an SSH private key, identifying
// themself by that key's SHA256 fingerprint rather than a typed username (see
// docs/PLAN.md's SSH-key-only login note).
//
// The fingerprint is not a secret -- it's a hash of a public key -- so
// putting it in the query string in the clear is no different from the
// username it replaced.
func (s *Server) requireSignature(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		fields := signedFields{
			Fingerprint: q.Get("fp"), Ts: q.Get("ts"), Nonce: q.Get("nonce"), Sig: q.Get("sig"),
		}
		if !fields.complete() {
			fail(w, r, "", http.StatusBadRequest, "fp, ts, nonce, sig are required")
			return
		}
		user := s.authorize(w, r, fields)
		if user == nil {
			return
		}
		next(w, r, user)
	}
}

// mfaRequest is the body of POST /v1/auth/mfa.
type mfaRequest struct {
	signedFields
	Code string `json:"code"`
}

// handleMFA verifies a TOTP code and, on success, records the resolved user
// as having completed MFA just now, resetting its cfg.MFAIntervalDays clock.
// The signature covers "fingerprint|ts|nonce|code" rather than just
// "fingerprint|ts|nonce" so a signature captured from a nodes/assets call
// can't be replayed here against an attacker-chosen code.
func (s *Server) handleMFA(w http.ResponseWriter, r *http.Request) {
	var req mfaRequest
	if !decodeSignedPost(w, r, &req) {
		return
	}
	if req.Code == "" {
		fail(w, r, "", http.StatusBadRequest, "code is required")
		return
	}
	// Not authorize(): this endpoint is how a lapsed caller gets back in, so
	// it verifies the signature without also demanding fresh MFA.
	user, err := s.verifySignedRequest(req.Fingerprint, req.Ts, req.Nonce, req.Sig, req.Code)
	if err != nil {
		fail(w, r, "", authFailureStatus(err), err.Error())
		return
	}
	// Checked before the code is even looked at, and keyed on the user rather
	// than the caller's address: whoever is guessing already holds a private
	// key that signs as this user, so the account is the only thing worth
	// rate-limiting -- and it is the one identifier they cannot vary.
	if remaining, locked := s.mfaFailures.LockedFor(user.Username); locked {
		fail(w, r, user.Username, http.StatusTooManyRequests, fmt.Sprintf(
			"too many incorrect MFA codes; try again in %s", remaining.Round(time.Second)))
		return
	}
	if user.OTPSecretKey == "" {
		// Not counted as a failure: nothing was guessed, the account simply has
		// no device enrolled, and locking it out would only bury the real cause.
		fail(w, r, user.Username, http.StatusBadRequest, "user has no MFA device enrolled in JumpServer")
		return
	}
	if !verifyTOTP(user.OTPSecretKey, req.Code) {
		if s.mfaFailures.Fail(user.Username) {
			// Logged unconditionally, unlike the individual wrong codes: a
			// lockout engaging is either someone fatfingering a code five times
			// or someone working through the keyspace with a stolen SSH key,
			// and the second one is worth having a timestamped record of.
			log.Printf("jmsd: mfa lockout: %s reached %d consecutive incorrect codes, refusing for %s",
				user.Username, mfaMaxFailures, mfaLockout)
		}
		fail(w, r, user.Username, http.StatusUnauthorized, "invalid code")
		return
	}
	// Cleared on success: the threshold counts CONSECUTIVE failures, so a
	// couple of mistyped codes must not accumulate across weeks into a lockout.
	s.mfaFailures.Reset(user.Username)
	// A failure to record must not answer 200. The code was correct, but
	// without the record the very next request finds MFA still stale and
	// re-prompts -- so reporting success would leave the user staring at a
	// prompt jmsd just told them they'd cleared.
	if err := s.state.RecordMFA(user.Username, time.Now()); err != nil {
		fail(w, r, user.Username, http.StatusServiceUnavailable, err.Error())
		return
	}
	// Naming the backend here is the answer to "did it actually persist?"
	// without anyone having to go read the store's keys by hand.
	note(r, user.Username, fmt.Sprintf("mfa verified, recorded in %s, next prompt in %dd",
		s.stateDesc, s.cfg.MFAIntervalDays))
	writeJSON(w, map[string]string{"status": "ok"})
}

// verifySignedRequest checks the timestamp window, replay via nonce, resolves
// fingerprint to a JumpServer user, and verifies the SSH signature over
// "fingerprint|ts|nonce" plus any extra fields in order, against that user's
// registered public keys. It returns the user record so callers get the
// JumpServer user id without a second lookup.
func (s *Server) verifySignedRequest(fingerprint, ts, nonce, sig string, extra ...string) (*userRecord, error) {
	tsSeconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid ts")
	}
	if age := time.Since(time.Unix(tsSeconds, 0)); age < -nonceWindow || age > nonceWindow {
		return nil, fmt.Errorf("timestamp outside allowed window")
	}
	seen, err := s.state.SeenNonce(nonce)
	if err != nil {
		return nil, err
	}
	if seen {
		return nil, fmt.Errorf("nonce already used")
	}

	// Served from userDirectory's cached index, so this is a map lookup rather
	// than the full decrypt-and-hash scan of every active key in the install
	// that it used to be. It still has to happen before the signature check --
	// there is no verifying a signature without first knowing whose keys to
	// try -- which is exactly why it must not touch the database per request.
	lookupDone := timed("resolve fingerprint %s", fingerprint)
	user, err := s.users.Lookup(fingerprint)
	lookupDone()
	if err != nil {
		return nil, err
	}
	if len(user.PublicKeys) == 0 {
		return nil, fmt.Errorf("user has no active SSH key registered in JumpServer")
	}

	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return nil, fmt.Errorf("invalid signature encoding")
	}
	var signature ssh.Signature
	if err := ssh.Unmarshal(sigBytes, &signature); err != nil {
		return nil, fmt.Errorf("invalid signature encoding")
	}

	payload := strings.Join(append([]string{fingerprint, ts, nonce}, extra...), "|")
	for _, raw := range user.PublicKeys {
		pubKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(raw))
		if err != nil {
			continue // skip one unparseable stored key rather than fail the request
		}
		if pubKey.Verify([]byte(payload), &signature) == nil {
			return user, nil
		}
	}
	return nil, fmt.Errorf("signature verification failed against every registered SSH key")
}

func (s *Server) mfaFresh(username string) (bool, error) {
	last, ok, err := s.state.LastMFA(username)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	// Compared here rather than left to the store's own expiry: the Redis
	// backend fixes an MFA key's TTL when it writes it, so shortening
	// mfa_interval_days afterwards would otherwise leave old keys honoured
	// for longer than the new setting allows.
	return time.Since(last) < time.Duration(s.cfg.MFAIntervalDays)*24*time.Hour, nil
}

// authFailureStatus separates "we could not authenticate you" from "we could
// not reach the state store to find out". Both arrive as an error from
// verifySignedRequest, but answering 401 for the second sends the caller
// hunting a credential problem they don't have -- and, in SeenNonce's case,
// would surface as "nonce already used", which reads as a replay attack.
func authFailureStatus(err error) int {
	if errors.Is(err, errStateUnavailable) {
		return http.StatusServiceUnavailable
	}
	return http.StatusUnauthorized
}

// whoamiResponse is the body of GET /v1/whoami. It's how jms learns its own
// username without the user ever typing it: the caller identifies itself by
// SSH key fingerprint (see requireSignature), and this just echoes back what
// jmsd resolved that fingerprint to, purely for display in the UI -- nothing
// downstream needs it, since every other endpoint identifies the caller by
// fingerprint too.
//
// It also carries jmsd's own version, which is how jms notices it is running a
// stale binary. This endpoint rather than a dedicated one because jms already
// calls it exactly once at startup and it is never served from the client's
// cache -- so the check costs no extra round trip and no extra latency.
type whoamiResponse struct {
	Username string `json:"username"`
	// ServerVersion is jmsd's build version. jms compares it against its own
	// and says so when they differ; an older jmsd omits the field entirely,
	// which jms reads as "this one can't tell me" and stays quiet rather than
	// reporting a mismatch it cannot substantiate.
	ServerVersion string `json:"server_version"`
	// JumpServerBaseline is the JumpServer release jmsd was verified against,
	// declared here rather than shared with the client so the two can actually
	// differ -- see JumpServerBaseline. Same empty-means-unknown rule as
	// ServerVersion.
	JumpServerBaseline string `json:"jumpserver_baseline"`
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request, user *userRecord) {
	writeJSON(w, whoamiResponse{
		Username:           user.Username,
		ServerVersion:      version.Version,
		JumpServerBaseline: JumpServerBaseline,
	})
}

// K8sConfigResponse is the body of GET /v1/k8s-config: the office network's
// front door to Koko's :5002 k8s proxy, read straight from config-jmsd.yaml
// (Config.K8s) -- one operator-controlled place instead of a setting every
// user's laptop has to carry (docs/K9S_LOCAL_PROXY_PLAN.md).
type K8sConfigResponse struct {
	ProxyURL           string `json:"proxy_url"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
}

func (s *Server) handleK8sConfig(w http.ResponseWriter, r *http.Request, user *userRecord) {
	resp := K8sConfigResponse{
		ProxyURL:           s.cfg.K8s.ProxyURL,
		InsecureSkipVerify: s.cfg.K8s.InsecureSkipVerify,
	}
	// Logged (unlike the other read endpoints) because this is the one place
	// an operator can tell, from jmsd's own log, whether a client actually
	// saw local-k9s mode as configured -- proxy_url isn't secret, it's just a
	// network address, and "is the config-jmsd.yaml edit even live yet" is
	// exactly the question this project has already needed to answer once.
	status := fmt.Sprintf("proxy=%s insecure_skip_verify=%t", resp.ProxyURL, resp.InsecureSkipVerify)
	if resp.ProxyURL == "" {
		status = "no k8s proxy configured -- k8s connections will fail"
	}
	note(r, user.Username, status)
	writeJSON(w, resp)
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request, user *userRecord) {
	nodesDone := timed("core api: GetUserNodes user=%s", user.ID)
	nodes, err := s.core.GetUserNodes(user.ID)
	nodesDone()
	if err != nil {
		fail(w, r, user.Username, http.StatusBadGateway, "load nodes: "+err.Error())
		return
	}
	writeJSON(w, BuildNodeTree(nodes))
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request, user *userRecord) {
	assets, err := s.userPermAssets(user.ID)
	if err != nil {
		fail(w, r, user.Username, http.StatusBadGateway, "load assets: "+err.Error())
		return
	}

	briefs := s.inventory.briefs(assets)
	rows := make([]AssetRow, 0, len(assets))
	for _, a := range assets {
		nodeIDs := make([]string, 0, len(a.Nodes))
		for _, n := range a.Nodes {
			nodeIDs = append(nodeIDs, n.ID)
		}
		rows = append(rows, AssetRow{
			ID:           a.ID,
			Name:         a.Name,
			Address:      a.Address,
			Category:     string(a.Category),
			Type:         string(a.Type),
			NodeIDs:      nodeIDs,
			CloudKey:     briefs[a.ID].Key,
			InstanceType: briefs[a.ID].InstanceType,
			OwnerTeam:    briefs[a.ID].OwnerTeam,
			AppModule:    briefs[a.ID].AppModule,
		})
	}
	writeJSON(w, rows)
}

// reqLog accumulates what a handler learned about a request so logging can
// emit exactly one line per request, after the fact, with the real status
// code and duration.
//
// Handlers used to log directly, which meant only refusals and a couple of
// writes ever appeared: /v1/whoami, /v1/nodes, /v1/assets and
// /v1/assets/detail printed nothing at all, so a client browsing assets left
// jmsd's log completely silent and there was no way to tell working from
// wedged.
type reqLog struct {
	user   string
	detail string
	status int
}

type reqLogKey struct{}

// note records who the caller turned out to be and what happened, for the
// single line logging prints when the handler returns. Either argument may be
// empty to leave that field as it was, so a handler can fill in detail
// without repeating the username authorize already resolved.
func note(r *http.Request, user, detail string) {
	entry, ok := r.Context().Value(reqLogKey{}).(*reqLog)
	if !ok {
		return
	}
	if user != "" {
		entry.user = user
	}
	if detail != "" {
		entry.detail = detail
	}
}

// logging wraps the whole mux so every request produces one line, including
// the ones no handler says anything about. It deliberately never logs the
// query string, which carries the caller's signature.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := &reqLog{status: http.StatusOK}
		started := time.Now()
		next.ServeHTTP(
			&statusRecorder{ResponseWriter: w, entry: entry},
			r.WithContext(context.WithValue(r.Context(), reqLogKey{}, entry)),
		)
		// Silent on a plain successful read. Logging every /v1/assets and
		// /v1/marks was pure noise -- the interesting events are failures and
		// the handful of writes, which say so by calling note with a detail.
		if entry.status == http.StatusOK && entry.detail == "" {
			return
		}
		user := entry.user
		if user == "" {
			user = "-"
		}
		detail := entry.detail
		if detail != "" {
			detail = " " + detail
		}
		log.Printf("jmsd: %s %s user=%s -> %d%s (%s)",
			r.Method, r.URL.Path, user, entry.status, detail,
			time.Since(started).Round(time.Millisecond))
	})
}

// statusRecorder remembers the status a handler wrote. A handler that writes
// a body without calling WriteHeader has implicitly sent 200, which is the
// zero state entry starts in.
type statusRecorder struct {
	http.ResponseWriter
	entry *reqLog
	wrote bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.entry.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func fail(w http.ResponseWriter, r *http.Request, user string, status int, msg string) {
	note(r, user, msg)
	writeJSONStatus(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
