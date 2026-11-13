package jmsd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jumpserver-dev/sdk-go/model"
	"github.com/lib/pq"
)

// runPostgres connects as token's account and runs query in a READ ONLY
// transaction with a server-side statement_timeout.
//
// sslmode: the asset's pg_ssl_mode when it asks for SSL, as Koko passes it.
// Otherwise SSL is tried first and plain only if the server has none -- RDS
// on recent versions refuses plain connections, and lib/pq has no "prefer".
func runPostgres(ctx context.Context, token *model.ConnectToken, dial dialFunc, query string) (QueryResult, error) {
	port := token.Asset.ProtocolPort("postgresql")
	if port == 0 {
		port = 5432
	}
	dbName := token.Asset.SpecInfo.DBName
	if dbName == "" {
		dbName = "postgres"
	}
	modes := []string{"require", "disable"}
	if token.Asset.SpecInfo.UseSSL && token.Asset.SpecInfo.PgSSLMode != "" {
		modes = []string{token.Asset.SpecInfo.PgSSLMode}
	}
	setup := fmt.Sprintf("SET LOCAL statement_timeout = %d", queryTimeout.Milliseconds())
	var err error
	for _, mode := range modes {
		dsn := url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(token.Account.Username, token.Account.Secret),
			Host:     net.JoinHostPort(token.Asset.Address, strconv.Itoa(port)),
			Path:     "/" + dbName,
			RawQuery: url.Values{"sslmode": {mode}, "connect_timeout": {"10"}}.Encode(),
		}
		connector, cerr := pq.NewConnector(dsn.String())
		if cerr != nil {
			return QueryResult{}, cerr
		}
		if dial != nil {
			connector.Dialer(pqDialer{dial})
		}
		var result QueryResult
		result, err = runReadOnly(ctx, connector, setup, query)
		if !errors.Is(err, pq.ErrSSLNotSupported) {
			return result, err
		}
	}
	return QueryResult{}, err
}

// pqDialer is a dialFunc with the two context-free methods lib/pq's Dialer
// interface requires; lib/pq calls DialContext when it is there.
type pqDialer struct{ dialFunc }

func (d pqDialer) Dial(network, addr string) (net.Conn, error) {
	return d.dialFunc(context.Background(), network, addr)
}

func (d pqDialer) DialTimeout(network, addr string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.dialFunc(ctx, network, addr)
}
