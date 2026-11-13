package jmsd

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestReadOnlyMongoCommand(t *testing.T) {
	for _, tc := range []struct {
		text string
		ok   bool
	}{
		{`{"find": "events", "filter": {"uid": 1}, "limit": 10}`, true},
		{`{"aggregate": "events", "pipeline": [{"$match": {"uid": 1}}], "cursor": {}}`, true},
		{`{"count": "events", "$db": "logs"}`, true},
		{`{"listDatabases": 1}`, true},
		{`{"aggregate": "events", "pipeline": [{"$out": "copy"}], "cursor": {}}`, false},
		{`{"aggregate": "events", "pipeline": [{"$merge": {"into": "x"}}], "cursor": {}}`, false},
		{`{"insert": "events", "documents": [{}]}`, false},
		{`{"update": "events", "updates": []}`, false},
		{`{"drop": "events"}`, false},
		{`{"filter": {}, "find": "events"}`, false}, // the command is the first key
		{`db.events.find({uid: 1})`, false},         // mongosh syntax is not JSON
		{`{}`, false},
	} {
		if _, err := readOnlyMongoCommand(tc.text); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.text, err, tc.ok)
		}
	}
}

// find and aggregate are capped one past queryMaxRows so a full result is
// told apart from a truncated one; a smaller limit the caller set stays.
func TestReadOnlyMongoCommandCapsResults(t *testing.T) {
	q, _ := readOnlyMongoCommand(`{"find": "e"}`)
	if n, _ := asInt(lookup(q.cmd, "limit")); n != queryMaxRows+1 {
		t.Errorf("find without limit: limit=%v", lookup(q.cmd, "limit"))
	}
	q, _ = readOnlyMongoCommand(`{"find": "e", "limit": 5}`)
	if n, _ := asInt(lookup(q.cmd, "limit")); n != 5 {
		t.Errorf("find limit 5 became %v", lookup(q.cmd, "limit"))
	}
	q, _ = readOnlyMongoCommand(`{"aggregate": "e", "pipeline": [], "cursor": {}}`)
	stages := lookup(q.cmd, "pipeline").(bson.A)
	if last := stages[len(stages)-1].(bson.D); last[0].Key != "$limit" {
		t.Errorf("aggregate pipeline not capped: %v", stages)
	}
	if q, _ = readOnlyMongoCommand(`{"count": "e", "$db": "logs"}`); q.db != "logs" || lookup(q.cmd, "$db") != nil {
		t.Errorf("$db not taken out: db=%q cmd=%v", q.db, q.cmd)
	}
}
