package jmsd

// Running one read-only SQL statement for a caller, the database half of what
// "jms -c ... -- command" does for hosts. Koko cannot serve it: its only
// non-interactive path takes ssh tickets, and for databases it runs an
// interactive usql it builds the connection string for itself.
//
// So jmsd connects itself, the way Koko does underneath: mint a ticket as the
// caller (Core checks their grant), trade it for the account's secret, run
// the statement. The secret lives for one request in this process and is
// never logged or returned. What was run is audited (action "query").

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jumpserver-dev/sdk-go/model"
	"golang.org/x/crypto/ssh"
)

const (
	queryTimeout  = 30 * time.Second
	queryMaxRows  = 1000
	queryMaxBytes = 1 << 20
)

// queryRequest is the body of POST /v1/query.
type queryRequest struct {
	signedFields
	Asset    string `json:"asset"`
	Account  string `json:"account"`
	Protocol string `json:"protocol"`
	SQL      string `json:"sql"`
}

// QueryResult is what /v1/query returns. A nil cell is SQL NULL.
type QueryResult struct {
	Columns   []string    `json:"columns"`
	Rows      [][]*string `json:"rows"`
	Truncated bool        `json:"truncated,omitempty"` // stopped at queryMaxRows or queryMaxBytes
	// Error is the database's own refusal (syntax, permission, timeout):
	// the request itself succeeded, so it is a 200 with this set, and jms
	// prints it the way the mysql client would.
	Error string `json:"error,omitempty"`

	size int // bytes in Rows so far, for add
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if !decodeSignedPost(w, r, &req) {
		return
	}
	if req.Asset == "" || req.Account == "" || req.SQL == "" {
		fail(w, r, "", http.StatusBadRequest, "asset, account, sql are required")
		return
	}
	user := s.authorize(w, r, req.signedFields, req.Asset, req.Account, req.Protocol, req.SQL)
	if user == nil {
		return
	}
	started := time.Now()
	ev := auditEvent{Timestamp: started, Action: auditQuery, Status: "ok", User: user.Username,
		Remote: remoteIP(r.RemoteAddr), Asset: req.Asset, Account: req.Account, Protocol: req.Protocol, SQL: req.SQL}
	refuse := func(status int, msg string) {
		ev.Status, ev.Reason, ev.Ms = "denied", msg, time.Since(started).Milliseconds()
		if status >= 500 {
			ev.Status = "error"
		}
		s.audit.send(ev)
		fail(w, r, user.Username, status, msg)
	}
	// Checked before anything is minted or dialled: a refused statement
	// costs no ticket and never reaches the database.
	run, err := prepareQuery(req.Protocol, req.SQL)
	if err != nil {
		refuse(http.StatusBadRequest, err.Error())
		return
	}

	// Minted as the caller, so Core applies their grant exactly as for a
	// Koko session -- and keeps its own record of who opened what.
	info, err := s.core.CreateSuperConnectToken(buildTokenRequest(user, req.Asset, req.Account, req.Protocol, r.RemoteAddr))
	if err != nil {
		refuse(http.StatusBadGateway, "mint connection token: "+err.Error())
		return
	}
	ev.Ticket = info.ID
	// expireNow: the ticket is used up here; nobody should be able to take
	// it to Koko afterwards.
	token, err := s.core.GetConnectTokenInfo(info.ID, true)
	if err != nil {
		refuse(http.StatusBadGateway, "load connection secret: "+err.Error())
		return
	}
	// Readable names over the ids the request carried: the alias jms sends
	// is the account's id, which means nothing to someone reading the audit.
	// Redis accounts often have no username (or JumpServer's "null"), so the
	// account's display name stands in.
	ev.AssetName, ev.Account = token.Asset.Name, token.Account.Name
	if u := token.Account.Username; u != "" && !token.Account.IsNull() {
		ev.Account = u
	}
	if t := token.Account.SecretType.Value; t != "" && t != "password" {
		refuse(http.StatusBadRequest, fmt.Sprintf("account %s uses a %s secret; only password accounts are supported", req.Account, t))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	// An asset in a JumpServer zone is reached through the gateway Core
	// picked for this ticket, exactly as Koko reaches it. nil dial = direct.
	var dial dialFunc
	if gw := token.Gateway; gw != nil {
		ev.Gateway = gw.Name
		client, err := dialGateway(ctx, gw)
		if err != nil {
			refuse(http.StatusBadGateway, fmt.Sprintf("gateway %s: %v", gw.Name, err))
			return
		}
		defer client.Close()
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := client.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return noDeadlineConn{conn}, nil
		}
	}
	result, err := run(ctx, &token, dial)
	ev.Ms = time.Since(started).Milliseconds()
	if err != nil {
		// The database answered (or could not be reached): reported to the
		// caller as the query's own error, the way a mysql client would.
		ev.Status, ev.Reason = "error", err.Error()
		s.audit.send(ev)
		note(r, user.Username, fmt.Sprintf("query asset=%s ticket=%s failed: %v", req.Asset, info.ID, err))
		writeJSON(w, QueryResult{Error: err.Error()})
		return
	}
	n := int64(len(result.Rows))
	ev.Rows = &n
	s.audit.send(ev)
	note(r, user.Username, fmt.Sprintf("query asset=%s ticket=%s rows=%d", req.Asset, info.ID, n))
	writeJSON(w, result)
}

// dialFunc reaches a database: through a zone gateway, or nil for direct.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// DialContext lets a dialFunc stand in for the drivers' dialer interfaces.
func (d dialFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d(ctx, network, addr)
}

// queryRun runs one checked query against the database token describes.
type queryRun func(ctx context.Context, token *model.ConnectToken, dial dialFunc) (QueryResult, error)

// prepareQuery checks text is a read for protocol and returns what runs it.
// The protocols here are exactly QueryProtocols (kind.go), which jms uses to
// decide what "-- command" means for an asset.
func prepareQuery(protocol, text string) (queryRun, error) {
	switch protocol {
	case "mysql", "postgresql":
		// PostgreSQL enforces READ ONLY for DDL too; MySQL does not (DDL
		// commits implicitly), which is why the verb check is the real gate.
		if err := readOnlyStatement(text); err != nil {
			return nil, err
		}
		if protocol == "postgresql" {
			return func(ctx context.Context, t *model.ConnectToken, d dialFunc) (QueryResult, error) {
				return runPostgres(ctx, t, d, text)
			}, nil
		}
		return func(ctx context.Context, t *model.ConnectToken, d dialFunc) (QueryResult, error) {
			return runMySQL(ctx, t, d, text)
		}, nil
	case "redis":
		args, err := readOnlyRedisCommand(text)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, t *model.ConnectToken, d dialFunc) (QueryResult, error) {
			return runRedis(ctx, t, d, args)
		}, nil
	case "mongodb":
		q, err := readOnlyMongoCommand(text)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, t *model.ConnectToken, d dialFunc) (QueryResult, error) {
			return runMongo(ctx, t, d, q)
		}, nil
	}
	return nil, fmt.Errorf("queries are supported over %s, not %q", strings.Join(QueryProtocols, ", "), protocol)
}

// add appends a row unless the result is already at queryMaxRows or past
// queryMaxBytes, in which case it marks the result truncated and reports
// false so the caller stops reading.
func (r *QueryResult) add(row []*string) bool {
	if len(r.Rows) >= queryMaxRows || r.size > queryMaxBytes {
		r.Truncated = true
		return false
	}
	for _, c := range row {
		if c != nil {
			r.size += len(*c)
		}
	}
	r.Rows = append(r.Rows, row)
	return true
}

// runMySQL connects as token's account, the way Koko would, and runs query.
func runMySQL(ctx context.Context, token *model.ConnectToken, dial dialFunc, query string) (QueryResult, error) {
	port := token.Asset.ProtocolPort("mysql")
	if port == 0 {
		port = 3306
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd = token.Account.Username, token.Account.Secret
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(token.Asset.Address, strconv.Itoa(port))
	cfg.DBName = token.Asset.SpecInfo.DBName
	cfg.Timeout, cfg.ReadTimeout = 10*time.Second, queryTimeout+5*time.Second
	if dial != nil {
		cfg.DialFunc = dial
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return QueryResult{}, err
	}
	// Server-side too, so a runaway SELECT stops on the database and not
	// only on our side of the socket. Not on every MySQL flavour; ignored
	// where unsupported, the context deadline still holds.
	return runReadOnly(ctx, connector, fmt.Sprintf("SET SESSION MAX_EXECUTION_TIME=%d", queryTimeout.Milliseconds()), query)
}

// runReadOnly runs sql inside a READ ONLY transaction that is always rolled
// back. The transaction is the second line of defence -- the first is that
// the account granted should only hold SELECT -- and readOnlyStatement is
// what stops DDL, which would commit implicitly and escape it.
//
// setup runs first inside the transaction (a server-side statement timeout);
// its failure is ignored.
func runReadOnly(ctx context.Context, connector driver.Connector, setup, query string) (QueryResult, error) {
	db := sql.OpenDB(connector)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return QueryResult{}, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return QueryResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	_, _ = tx.ExecContext(ctx, setup)

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return QueryResult{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return QueryResult{}, err
	}
	out := QueryResult{Columns: cols, Rows: [][]*string{}}
	raw := make([]sql.RawBytes, len(cols))
	dest := make([]any, len(cols))
	for i := range raw {
		dest[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return QueryResult{}, err
		}
		row := make([]*string, len(cols))
		for i, b := range raw {
			if b != nil {
				v := string(b)
				row[i] = &v
			}
		}
		if !out.add(row) {
			break
		}
	}
	return out, rows.Err()
}

var (
	leadingComments = regexp.MustCompile(`(?s)^(\s+|/\*.*?\*/|(--|#)[^\n]*\n?)+`)
	// readOnlyVerbs are statements that cannot change data. Everything else
	// is refused before a connection is made, DDL above all: MySQL commits
	// DDL implicitly, so a READ ONLY transaction would not contain it.
	readOnlyVerbs = map[string]bool{"select": true, "show": true, "describe": true, "desc": true, "explain": true, "with": true}
	intoFile      = regexp.MustCompile(`(?i)\binto\s+(out|dump)file\b`)
)

func readOnlyStatement(query string) error {
	q := leadingComments.ReplaceAllString(query, "")
	words := strings.FieldsFunc(q, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	})
	verb := ""
	if len(words) > 0 {
		verb = strings.ToLower(words[0])
	}
	if !readOnlyVerbs[verb] {
		return fmt.Errorf("only SELECT, SHOW, DESCRIBE, EXPLAIN and WITH statements are allowed, not %q", verb)
	}
	if intoFile.MatchString(q) {
		return errors.New("SELECT ... INTO OUTFILE/DUMPFILE is not allowed")
	}
	// A second statement after ";" is refused by the driver itself
	// (multiStatements is off), so it needs no check here.
	return nil
}

// dialGateway logs in to a zone's gateway the way Koko does
// (pkg/proxy/domain_gateway.go createGatewaySSHClient): its account's key or
// password, and no host key check -- JumpServer stores none to check against,
// and this matches the risk Koko already runs.
//
// Bounded by ctx as a whole, handshake included: ssh.ClientConfig.Timeout
// covers only the TCP connect, and a gateway that accepts the socket but never
// speaks would otherwise hold the request forever.
func dialGateway(ctx context.Context, gw *model.Gateway) (*ssh.Client, error) {
	var auth []ssh.AuthMethod
	if gw.Account.IsSSHKey() {
		signer, err := ssh.ParsePrivateKey([]byte(gw.Account.Secret))
		if err != nil {
			return nil, fmt.Errorf("parse gateway key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	} else {
		secret := gw.Account.Secret
		auth = append(auth, ssh.Password(secret), ssh.KeyboardInteractive(
			func(string, string, []string, []bool) ([]string, error) { return []string{secret}, nil }))
	}
	port := gw.Protocols.GetProtocolPort("ssh")
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(gw.Address, strconv.Itoa(port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            gw.Account.Username,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{}) // the query's own timeouts take over from here
	return ssh.NewClient(c, chans, reqs), nil
}

// noDeadlineConn is a connection forwarded through a gateway. The MySQL
// driver sets read/write deadlines on every packet and treats a failure as a
// dead connection ("invalid connection"); an SSH channel supports none. The
// deadlines are dropped: the query context, MAX_EXECUTION_TIME and closing
// the gateway client are what bound the query instead.
type noDeadlineConn struct{ net.Conn }

func (noDeadlineConn) SetDeadline(time.Time) error      { return nil }
func (noDeadlineConn) SetReadDeadline(time.Time) error  { return nil }
func (noDeadlineConn) SetWriteDeadline(time.Time) error { return nil }
