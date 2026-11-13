package jmsd

import "testing"

// What reaches the database. DDL is the case that matters most: MySQL commits
// it implicitly, so the READ ONLY transaction would not stop it.
func TestReadOnlyStatement(t *testing.T) {
	for _, tc := range []struct {
		sql string
		ok  bool
	}{
		{"select 1", true},
		{"  SELECT * FROM t WHERE a = ';'", true},
		{"/* hi */ show tables", true},
		{"-- note\nexplain select 1", true},
		{"# note\ndesc t", true},
		{"with x as (select 1) select * from x", true},
		{"drop table t", false},
		{"/* select */ delete from t", false},
		{"update t set a=1", false},
		{"truncate t", false},
		{"select * from t into outfile '/tmp/x'", false},
		{"", false},
		{"   ", false},
	} {
		if err := readOnlyStatement(tc.sql); (err == nil) != tc.ok {
			t.Errorf("readOnlyStatement(%q) = %v, want ok=%v", tc.sql, err, tc.ok)
		}
	}
}
