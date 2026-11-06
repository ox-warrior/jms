package jmsd

import (
	"reflect"
	"testing"
)

func TestReadOnlyRedisCommand(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []any // nil = refused
	}{
		{`GET user:1`, []any{"GET", "user:1"}},
		{`hget "a key" 'f 1'`, []any{"hget", "a key", "f 1"}},
		{`get "q\"uote"`, []any{"get", `q"uote`}},
		{`MEMORY USAGE k`, []any{"MEMORY", "USAGE", "k"}},
		{`SCAN 0 MATCH user:* COUNT 100`, []any{"SCAN", "0", "MATCH", "user:*", "COUNT", "100"}},
		{`KEYS *`, nil},       // stalls the server
		{`CONFIG GET *`, nil}, // returns requirepass
		{`MEMORY PURGE`, nil}, // only USAGE
		{`SET k v`, nil},
		{`DEL k`, nil},
		{`FLUSHALL`, nil},
		{`EVAL "return 1" 0`, nil},
		{`get "open`, nil},
		{``, nil},
	} {
		got, err := readOnlyRedisCommand(tc.line)
		if tc.want == nil {
			if err == nil {
				t.Errorf("%q: allowed, want refused", tc.line)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q = %v, %v; want %v", tc.line, got, err, tc.want)
		}
	}
}

// A value with newlines stays one cell; only INFO is split into lines.
func TestRedisRows(t *testing.T) {
	if got := redisRows("a\nb", false); len(got.Rows) != 1 || *got.Rows[0][0] != "a\nb" {
		t.Errorf("GET-style reply split: %v", got.Rows)
	}
	if got := redisRows("# Server\r\nredis_version:7\r\n", true); len(got.Rows) != 2 || *got.Rows[1][0] != "redis_version:7" {
		t.Errorf("INFO reply = %v", got.Rows)
	}
	got := redisRows([]any{"0", []any{"k1", "k2"}, nil, int64(3)}, false)
	if len(got.Rows) != 4 || *got.Rows[1][0] != `["k1","k2"]` || got.Rows[2][0] != nil || *got.Rows[3][0] != "3" {
		t.Errorf("array reply = %v", got.Rows)
	}
}
