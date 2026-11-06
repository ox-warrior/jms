package jmsd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/jumpserver-dev/sdk-go/model"
	"github.com/redis/go-redis/v9"
)

// redisReadCommands are the commands a query may run: reads that touch a
// bounded amount of data. KEYS is the notable absence -- it walks the whole
// keyspace on the server's single thread and stalls a production instance;
// SCAN is the answer. CONFIG GET is out too: it hands back requirepass.
var redisReadCommands = map[string]bool{
	"get": true, "mget": true, "strlen": true, "getrange": true, "exists": true, "type": true, "ttl": true, "pttl": true,
	"hget": true, "hmget": true, "hgetall": true, "hkeys": true, "hvals": true, "hlen": true, "hexists": true, "hstrlen": true,
	"lrange": true, "llen": true, "lindex": true,
	"smembers": true, "scard": true, "sismember": true, "smismember": true, "srandmember": true,
	"zrange": true, "zrangebyscore": true, "zrevrange": true, "zrevrangebyscore": true, "zcard": true,
	"zscore": true, "zrank": true, "zrevrank": true, "zcount": true,
	"xrange": true, "xrevrange": true, "xlen": true,
	"scan": true, "hscan": true, "sscan": true, "zscan": true,
	"info": true, "dbsize": true, "ping": true,
	// Two-word commands: only these subcommands.
	"memory usage": true, "slowlog get": true, "slowlog len": true, "object encoding": true,
}

// readOnlyRedisCommand splits a redis-cli style line into arguments and
// refuses anything not in redisReadCommands.
func readOnlyRedisCommand(line string) ([]any, error) {
	words, err := splitRedisLine(line)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, errors.New("empty command")
	}
	name := strings.ToLower(words[0])
	// MEMORY, SLOWLOG and OBJECT are allowed per subcommand only.
	if (name == "memory" || name == "slowlog" || name == "object") && len(words) > 1 {
		name += " " + strings.ToLower(words[1])
	}
	if !redisReadCommands[name] {
		return nil, fmt.Errorf("redis command %q is not allowed; only reads such as GET, HGETALL, SCAN, TTL, INFO are (KEYS is not: use SCAN)", name)
	}
	args := make([]any, len(words))
	for i, w := range words {
		args[i] = w
	}
	return args, nil
}

// splitRedisLine splits on spaces, honouring '...' and "..." quoting and
// backslash escapes inside double quotes -- enough of redis-cli's syntax for
// keys with spaces or colons.
func splitRedisLine(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for i := 0; i < len(line); i++ {
		c := rune(line[i])
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote == '"' && c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case quote != 0:
			cur.WriteRune(c)
		case c == '"' || c == '\'':
			quote, inWord = c, true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words, inWord = append(words, cur.String()), false
				cur.Reset()
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unbalanced quotes")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// runRedis connects as token's account with the same rules Koko applies
// (pkg/proxy/server.go getRedisConn): the username is sent only when the
// platform says to authenticate with one and it is not JumpServer's "null"
// placeholder; TLS when the asset asks for it, unverified as in Koko.
//
// ponytail: one node only. A cluster-mode instance answers keys on other
// shards with MOVED, which comes back as the error; follow redirects if
// that turns out to matter.
func runRedis(ctx context.Context, token *model.ConnectToken, dial dialFunc, args []any) (QueryResult, error) {
	port := token.Asset.ProtocolPort("redis")
	if port == 0 {
		port = 6379
	}
	username := token.Account.Username
	if p, ok := token.Platform.GetProtocolSetting("redis"); !ok || !p.GetSetting().AuthUsername || token.Account.IsNull() {
		username = ""
	}
	db, _ := strconv.Atoi(token.Asset.SpecInfo.DBName)
	opts := &redis.Options{
		Addr:     net.JoinHostPort(token.Asset.Address, strconv.Itoa(port)),
		Username: username, Password: token.Account.Secret, DB: db,
		Protocol: 2, // flat arrays: HGETALL as field, value, field, value -- what redis-cli prints
		// One command, one connection: no pool to keep, no retry that could
		// run a command twice.
		PoolSize: 1, MaxRetries: -1,
		ReadTimeout: queryTimeout, WriteTimeout: queryTimeout,
		DisableIdentity: true, // skip CLIENT SETINFO, which older servers reject
	}
	if dial != nil {
		opts.Dialer = dial
	}
	if token.Asset.SpecInfo.UseSSL {
		opts.TLSConfig = &tls.Config{InsecureSkipVerify: true, ServerName: token.Asset.Address}
	}
	client := redis.NewClient(opts)
	defer client.Close()

	// ponytail: the reply is read whole before redisRows caps it, so an
	// LRANGE 0 -1 or HGETALL on a huge key costs jmsd that much memory once.
	// Clamp range arguments if that ever bites.
	reply, err := client.Do(ctx, args...).Result()
	if errors.Is(err, redis.Nil) {
		return QueryResult{Rows: [][]*string{{nil}}}, nil
	}
	if err != nil {
		return QueryResult{}, err
	}
	// INFO is the one reply that is a document rather than a value; a GET
	// whose value holds newlines must stay one cell.
	return redisRows(reply, strings.EqualFold(fmt.Sprint(args[0]), "info")), nil
}

// redisRows lays a reply out one value per row, as redis-cli --raw does.
// lines splits a text reply into its lines (INFO); an element that is
// itself a list (SCAN's cursor + keys, XRANGE entries) is written as JSON on
// its row rather than flattened, so structure is not lost.
func redisRows(reply any, lines bool) QueryResult {
	out := QueryResult{Rows: [][]*string{}}
	switch v := reply.(type) {
	case []any:
		for _, item := range v {
			if !out.add([]*string{redisCell(item)}) {
				break
			}
		}
	case string:
		if !lines {
			out.add([]*string{&v})
			break
		}
		for _, line := range strings.Split(strings.TrimRight(v, "\r\n"), "\n") {
			line = strings.TrimRight(line, "\r")
			if !out.add([]*string{&line}) {
				break
			}
		}
	default:
		out.add([]*string{redisCell(v)})
	}
	return out
}

func redisCell(v any) *string {
	var s string
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		s = x
	case []any:
		b, _ := json.Marshal(x)
		s = string(b)
	default:
		s = fmt.Sprint(x)
	}
	return &s
}
