package jmsd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jumpserver-dev/sdk-go/model"
	"github.com/jumpserver-dev/sdk-go/service"
)

// AccountOption is one account a user may connect as, returned by
// GET /v1/assets/detail. Alias is what gets sent back in a later
// POST /v1/connections -- Core matches accounts by alias, not name or id
// (docs/PLAN.md §5, apps/perms/utils/asset_perm.py's accounts_mapper).
type AccountOption struct {
	Alias    string `json:"alias"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

// ProtocolOption is one protocol available for an asset, already filtered to
// what Koko can serve as a terminal session (docs/PLAN.md §5.A.5) -- no
// rdp/vnc, this client only ever drives Koko over SSH.
type ProtocolOption struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// AssetDetail is the response body of GET /v1/assets/detail.
type AssetDetail struct {
	Accounts  []AccountOption  `json:"accounts"`
	Protocols []ProtocolOption `json:"protocols"`
}

// ConnectionTicket is the response body of POST /v1/connections. It mirrors
// sdk-go's model.ConnectTokenInfo and deliberately excludes the target
// asset's secret: that never leaves Core (docs/PLAN.md §4).
type ConnectionTicket struct {
	ID          string `json:"id"`
	Value       string `json:"value"`
	ExpireTime  int    `json:"expire_time"`
	AccountName string `json:"account_name"`
	Protocol    string `json:"protocol"`
}

// kokoProtocols are the protocols Koko can serve as a terminal session
// (docs/PLAN.md §5.A.5). Anything else (rdp, vnc, ...) is a GUI protocol
// this client has no way to drive.
var kokoProtocols = map[string]bool{
	"ssh": true, "telnet": true, "sftp": true,
	"mysql": true, "mariadb": true, "postgresql": true,
	"oracle": true, "sqlserver": true, "redis": true,
	"mongodb": true, "k8s": true, "clickhouse": true,
}

// inputAccountAlias is JumpServer's alias for "manually type a
// username/password at connect time" (apps/accounts/const/account.py:15).
// Handling it needs an extra input step this client doesn't have yet, so
// toAccountOptions filters it out rather than offer an option that would
// fail with input_username/input_secret silently left blank.
const inputAccountAlias = "@INPUT"

// filterKokoProtocols keeps only the protocols Koko can serve as a terminal
// session, in the order Core returned them.
func filterKokoProtocols(protocols []model.Protocol) []ProtocolOption {
	out := make([]ProtocolOption, 0, len(protocols))
	for _, p := range protocols {
		if kokoProtocols[strings.ToLower(p.Name)] {
			out = append(out, ProtocolOption{Name: p.Name, Port: p.Port})
		}
	}
	return out
}

// toAccountOptions converts Core's permed-account list, dropping the
// manual-input alias (see inputAccountAlias).
func toAccountOptions(accounts []model.PermAccount) []AccountOption {
	out := make([]AccountOption, 0, len(accounts))
	for _, a := range accounts {
		if a.Alias == inputAccountAlias {
			continue
		}
		out = append(out, AccountOption{Alias: a.Alias, Name: a.Name, Username: a.Username})
	}
	return out
}

// handleAssetDetail answers GET /v1/assets/detail?asset=<id>, listing the
// accounts and Koko-compatible protocols the caller may connect to that
// asset with. It runs behind requireSignature like /v1/assets: this is a
// read, so (unlike /v1/connections) the asset id isn't bound into the
// signature.
func (s *Server) handleAssetDetail(w http.ResponseWriter, r *http.Request, user *userRecord) {
	assetID := r.URL.Query().Get("asset")
	if assetID == "" {
		fail(w, r, user.Username, http.StatusBadRequest, "asset is required")
		return
	}
	detailDone := timed("core api: GetUserPermAssetDetailById asset=%s user=%s", assetID, user.ID)
	detail, err := s.core.GetUserPermAssetDetailById(user.ID, assetID)
	detailDone()
	if err != nil {
		fail(w, r, user.Username, http.StatusBadGateway, "load asset detail: "+err.Error())
		return
	}
	out := AssetDetail{
		Accounts:  toAccountOptions(detail.PermedAccounts),
		Protocols: filterKokoProtocols(detail.PermedProtocols),
	}
	// Only worth a line when the answer is unusable: an empty list here is a
	// common cause of "I can't connect to this thing", and the filtering that
	// produced it (inputAccountAlias, kokoProtocols) is invisible client-side.
	// A normal answer says nothing.
	if len(out.Accounts) == 0 || len(out.Protocols) == 0 {
		note(r, "", fmt.Sprintf("asset=%s has %d permitted accounts and %d koko-capable protocols (of %d) -- client cannot connect",
			assetID, len(out.Accounts), len(out.Protocols), len(detail.PermedProtocols)))
	}
	writeJSON(w, out)
}

// connectionRequest is the body of POST /v1/connections.
type connectionRequest struct {
	signedFields
	Asset    string `json:"asset"`
	Account  string `json:"account"`
	Protocol string `json:"protocol"`
	// Command is the remote command a "jms -- command" run mints the ticket
	// for, empty for an interactive session. Audited only: the ticket is not
	// bound to it (Koko accepts any command on it), so it records what the
	// caller declared, and Koko's own command log in Core is the authority.
	Command string `json:"command,omitempty"`
}

// connectMethod is sent as SuperConnectTokenReq.ConnectMethod. The field is
// required (Django CharField, no blank=True, no default) but has no choices
// validation and Koko never reads it (docs/PLAN.md §5/§6), so a single fixed
// value covers every protocol this client mints a ticket for.
const connectMethod = "ssh_client"

// handleConnections mints a super-connection-token (docs/PLAN.md §4 阶段3).
// Unlike /v1/nodes and /v1/assets it does not go through the requireSignature
// middleware: the signature must bind asset|account|protocol -- the same
// reason /v1/auth/google binds the token -- so a signature captured from a read
// call can't be replayed here to mint a ticket for an attacker-chosen asset.
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	var req connectionRequest
	if !decodeSignedPost(w, r, &req) {
		return
	}
	if req.Asset == "" || req.Account == "" || req.Protocol == "" {
		fail(w, r, "", http.StatusBadRequest, "asset, account, protocol are required")
		return
	}
	extra := []string{req.Asset, req.Account, req.Protocol}
	if req.Command != "" {
		extra = append(extra, req.Command) // signed, so it cannot be swapped in transit
	}
	user := s.authorize(w, r, req.signedFields, extra...)
	if user == nil {
		return
	}

	mintDone := timed("core api: CreateSuperConnectToken asset=%s account=%s", req.Asset, req.Account)
	info, err := s.core.CreateSuperConnectToken(
		buildTokenRequest(user, req.Asset, req.Account, req.Protocol, r.RemoteAddr))
	mintDone()
	ev := auditEvent{Action: auditTicket, Status: "ok", User: user.Username, Remote: remoteIP(r.RemoteAddr),
		Asset: req.Asset, Account: req.Account, Protocol: req.Protocol, Ticket: info.ID, Command: req.Command}
	if err != nil {
		ev.Status, ev.Reason = "error", err.Error()
		s.audit.send(ev)
		fail(w, r, user.Username, http.StatusBadGateway, "mint connection token: "+err.Error())
		return
	}
	if info.AccountName != "" { // Core does not always return it; the id beats nothing
		ev.Account = info.AccountName
	}
	s.audit.send(ev)
	// ticket_id is included so it can be cross-referenced against Core's
	// terminal_session and, for k8s assets, the cluster's own Audit Log when
	// tracing who did what (docs/K9S_LOCAL_PROXY_PLAN.md §10).
	note(r, user.Username, fmt.Sprintf(
		"minted connection token ticket=%s asset=%s account=%s protocol=%s remote=%s",
		info.ID, req.Asset, req.Account, req.Protocol, r.RemoteAddr,
	))
	writeJSON(w, ConnectionTicket{
		ID: info.ID, Value: info.Value, ExpireTime: info.ExpireTime,
		AccountName: info.AccountName, Protocol: info.Protocol,
	})
}

// buildTokenRequest is split out from handleConnections so the exit check --
// a ticket is always minted for the hop①-verified caller, never a
// client-supplied identity -- has a unit test that doesn't need a live Core
// connection (docs/PLAN.md §10, "出口检查...这一条要有单元测试").
//
// remoteAddr is the calling jms's address, forwarded so Core's own connection
// token record carries it. It was left blank, which meant jmsd's log was the
// only place a ticket could be traced back to a machine -- and the JumpServer
// console, the thing an auditor actually opens, showed no source at all
// (docs/PLAN.md §10.2's "审计双写" has to be legible on both sides to be worth
// anything).
func buildTokenRequest(user *userRecord, assetID, account, protocol, remoteAddr string) *service.SuperConnectTokenReq {
	return &service.SuperConnectTokenReq{
		UserId:        user.ID,
		AssetId:       assetID,
		Account:       account,
		Protocol:      protocol,
		ConnectMethod: connectMethod,
		RemoteAddr:    remoteAddr,
	}
}
