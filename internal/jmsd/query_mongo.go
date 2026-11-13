package jmsd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/jumpserver-dev/sdk-go/model"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoReadCommands are the database commands a query may run. A query is a
// command document in MongoDB extended JSON -- {"find": "events", "filter":
// {...}} -- rather than mongosh's db.events.find(...), which is JavaScript and
// would need a JavaScript parser to take apart safely.
var mongoReadCommands = map[string]bool{
	"find": true, "aggregate": true, "count": true, "distinct": true,
	"listCollections": true, "listIndexes": true, "listDatabases": true,
	"dbStats": true, "collStats": true, "serverStatus": true,
	"ping": true, "buildInfo": true, "hello": true, "replSetGetStatus": true,
}

// mongoQuery is a parsed, checked command: what to run and against which
// database ("" = the asset's own).
type mongoQuery struct {
	cmd bson.D
	db  string
}

// readOnlyMongoCommand parses text and refuses anything that could write.
// The command name is the document's first key, as MongoDB reads it.
func readOnlyMongoCommand(text string) (mongoQuery, error) {
	var cmd bson.D
	if err := bson.UnmarshalExtJSON([]byte(text), false, &cmd); err != nil {
		return mongoQuery{}, fmt.Errorf(`a MongoDB query is a command document in JSON, e.g. {"find": "events", "filter": {"uid": 1}, "limit": 10}: %w`, err)
	}
	if len(cmd) == 0 {
		return mongoQuery{}, errors.New("empty command document")
	}
	q := mongoQuery{}
	// "$db" picks the database, as in MongoDB's own wire format.
	for i, e := range cmd {
		if e.Key == "$db" {
			q.db, _ = e.Value.(string)
			cmd = append(cmd[:i], cmd[i+1:]...)
			break
		}
	}
	name := cmd[0].Key
	if !mongoReadCommands[name] {
		return mongoQuery{}, fmt.Errorf("MongoDB command %q is not allowed; only reads such as find, aggregate, count, distinct, listCollections, dbStats are", name)
	}
	switch name {
	case "aggregate":
		// The two stages that write: they would turn a read into a
		// collection replace or a merge.
		pipeline, _ := lookup(cmd, "pipeline").(bson.A)
		for _, stage := range pipeline {
			if d, ok := stage.(bson.D); ok && len(d) > 0 && (d[0].Key == "$out" || d[0].Key == "$merge") {
				return mongoQuery{}, fmt.Errorf("aggregate stage %s writes and is not allowed", d[0].Key)
			}
		}
		// One more than the cap, so a result that reached it is told apart
		// from one that is exactly that long.
		cmd = set(cmd, "pipeline", append(pipeline, bson.D{{Key: "$limit", Value: queryMaxRows + 1}}))
		cmd = set(cmd, "cursor", bson.D{{Key: "batchSize", Value: queryMaxRows + 1}})
	case "find":
		if n, ok := asInt(lookup(cmd, "limit")); !ok || n <= 0 || n > queryMaxRows {
			cmd = set(cmd, "limit", queryMaxRows+1)
		}
		cmd = set(cmd, "batchSize", queryMaxRows+1)
	case "listDatabases":
		q.db = "admin"
	}
	q.cmd = cmd
	return q, nil
}

// runMongo connects as token's account with Koko's URI rules
// (pkg/srvconn/conn_mongodb.go): authSource from the platform setting,
// default admin; the platform's extra connection options appended; TLS,
// unverified, when the asset asks for it.
func runMongo(ctx context.Context, token *model.ConnectToken, dial dialFunc, q mongoQuery) (QueryResult, error) {
	port := token.Asset.ProtocolPort("mongodb")
	if port == 0 {
		port = 27017
	}
	params := url.Values{"authSource": {"admin"}}
	if p, ok := token.Platform.GetProtocolSetting("mongodb"); ok {
		s := p.GetSetting()
		if s.AuthSource != "" {
			params.Set("authSource", s.AuthSource)
		}
		for _, kv := range strings.Split(s.ConnectionOpts, "&") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				params.Set(k, v)
			}
		}
	}
	uri := url.URL{
		Scheme:   "mongodb",
		User:     url.UserPassword(token.Account.Username, token.Account.Secret),
		Host:     net.JoinHostPort(token.Asset.Address, strconv.Itoa(port)),
		Path:     "/" + token.Asset.SpecInfo.DBName,
		RawQuery: params.Encode(),
	}
	opts := options.Client().ApplyURI(uri.String()).
		SetConnectTimeout(queryTimeout).SetServerSelectionTimeout(queryTimeout).
		SetRetryReads(false) // one command runs once
	if token.Asset.SpecInfo.UseSSL {
		opts.SetTLSConfig(&tls.Config{InsecureSkipVerify: true})
	}
	if dial != nil {
		opts.SetDialer(dial)
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return QueryResult{}, err
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	db := q.db
	if db == "" {
		db = token.Asset.SpecInfo.DBName
	}
	if db == "" {
		db = "admin"
	}
	reply, err := client.Database(db).RunCommand(ctx, q.cmd).Raw()
	if err != nil {
		return QueryResult{}, err
	}
	return mongoRows(reply), nil
}

// mongoRows writes one document per line as relaxed extended JSON, so the
// output feeds jq. A cursor reply (find, aggregate, listCollections) is laid
// out as its documents; anything else is the reply itself on one line.
func mongoRows(reply bson.Raw) QueryResult {
	out := QueryResult{Rows: [][]*string{}}
	docs := []bson.Raw{reply}
	if batch, err := reply.LookupErr("cursor", "firstBatch"); err == nil {
		values, _ := batch.Array().Values()
		docs = docs[:0]
		for _, v := range values {
			docs = append(docs, v.Document())
		}
		// A cursor still open means the server had more than it sent.
		if id, err := reply.LookupErr("cursor", "id"); err == nil && id.AsInt64() != 0 {
			out.Truncated = true
		}
	}
	for _, d := range docs {
		b, err := bson.MarshalExtJSON(d, false, false)
		if err != nil {
			b = []byte(d.String())
		}
		s := string(b)
		if !out.add([]*string{&s}) {
			break
		}
	}
	return out
}

func lookup(d bson.D, key string) any {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}

// set replaces key's value, or appends it -- never before the first key,
// which is the command name.
func set(d bson.D, key string, value any) bson.D {
	for i, e := range d {
		if e.Key == key {
			d[i].Value = value
			return d
		}
	}
	return append(d, bson.E{Key: key, Value: value})
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}
