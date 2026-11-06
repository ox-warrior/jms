package jmsd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// The mapping is strict: an event field it does not list makes ES reject the
// whole document, which would only show up as a log line on the jmsd host.
func TestAuditEventFieldsAreAllMapped(t *testing.T) {
	props := auditMapping["properties"].(map[string]any)
	typ := reflect.TypeOf(auditEvent{})
	for i := range typ.NumField() {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if _, ok := props[name]; !ok {
			t.Errorf("auditEvent.%s (%q) is not in auditMapping", typ.Field(i).Name, name)
		}
	}
}

// Setup goes in before the first write, and a document lands in the index for
// its Beijing-time day -- a UTC day would split one local day across two.
func TestAuditShipperSetsUpThenBulkWritesByBeijingDay(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	bulk := make(chan string, 1)
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/_bulk" {
			_, _ = w.Write([]byte(`{"errors":false,"items":[]}`))
			bulk <- string(body)
			return
		}
		_, _ = w.Write([]byte(`{"acknowledged":true}`))
	}))
	defer es.Close()

	a := newAuditShipper(es.URL, "jmsd-test")
	// 2026-11-06 20:00 UTC is already 11-07 in Beijing.
	a.send(auditEvent{Timestamp: time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC), Action: auditQuery, Status: "ok", User: "alice"})

	var body string
	select {
	case body = <-bulk:
	case <-time.After(auditWait + 5*time.Second):
		t.Fatal("no bulk write")
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"jmsd-audit-2026-11-07"`) {
		t.Fatalf("bulk body = %q; want one doc in jmsd-audit-2026-11-07", body)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &doc); err != nil || doc["user"] != "alice" || doc["host"] != "jmsd-test" {
		t.Fatalf("doc = %v (%v)", doc, err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"PUT /_ilm/policy/jmsd-audit-30d", "PUT /_index_template/jmsd-audit", "PUT /jmsd-audit-*/_mapping", "POST /_bulk"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestAuditShipperNilIsANoop(t *testing.T) {
	var a *auditShipper
	a.send(auditEvent{Action: auditQuery}) // must not panic
}
