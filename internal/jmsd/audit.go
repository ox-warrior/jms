package jmsd

// The audit trail of what people did through jmsd, shipped to Elasticsearch
// the way sre-triage-mcp ships its own (core.py AuditShipper): one document
// per action, bulk-written from a background goroutine so a slow or absent ES
// never delays a request, into one index per Beijing-time day kept 30 days.
//
// It exists because Core sees only part of it: a ticket, but not the SQL run
// with it, and nothing at all of a tunnel or a sign-in.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	auditName   = "jmsd-audit"
	auditPrefix = auditName + "-"
	auditBatch  = 500
	auditWait   = 5 * time.Second
)

// Audit actions. One index holds them all so "what did X do through jmsd" is
// one query.
const (
	auditQuery        = "query"         // a SQL statement run for someone
	auditTicket       = "ticket"        // a connection ticket minted (ssh, k8s, ...)
	auditSignin       = "signin"        // a Google sign-in, bound or refused
	auditTunnelOpen   = "tunnel.open"   // a tunnel connection authenticated
	auditTunnelClose  = "tunnel.close"  // ... and ended
	auditTunnelStream = "tunnel.stream" // one stream through it, or one refused
)

// auditEvent is one document. A struct rather than a map because the index
// mapping is strict: a field the template does not know would be rejected,
// and the compiler is what keeps the two in step (see auditMapping).
type auditEvent struct {
	Timestamp time.Time `json:"@timestamp"`
	Action    string    `json:"action"`
	Status    string    `json:"status"` // ok | denied | error
	User      string    `json:"user,omitempty"`
	// OnBehalfOf: the person a service key acted for, as the key's holder
	// (e.g. the sre-triage gateway) vouched. User is still whose grants applied.
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	Remote     string `json:"remote,omitempty"` // the caller's address
	Ms         int64  `json:"ms,omitempty"`
	Reason     string `json:"reason,omitempty"` // why it was denied or failed
	Host       string `json:"host,omitempty"`   // which jmsd

	// What it was aimed at.
	Asset     string `json:"asset,omitempty"`      // asset id
	AssetName string `json:"asset_name,omitempty"` // when jmsd knows it (Core returns it with a query's secret)
	Account   string `json:"account,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	Ticket    string `json:"ticket,omitempty"`  // joins this to Core's own record of the ticket
	Gateway   string `json:"gateway,omitempty"` // the zone gateway a query went through

	// ticket: the remote command it was minted for, if any
	Command string `json:"command,omitempty"`

	// query
	SQL  string `json:"sql,omitempty"`
	Rows *int64 `json:"rows,omitempty"` // pointer: zero rows is an answer, absent is not

	// signin
	Email       string `json:"email,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`

	// tunnel
	Requested string `json:"requested,omitempty"` // what the client asked for, name:port
	Target    string `json:"target,omitempty"`    // what jmsd dialled, ip:port
	BytesUp   int64  `json:"bytes_up,omitempty"`
	BytesDown int64  `json:"bytes_down,omitempty"`
	Streams   int64  `json:"streams,omitempty"`
}

var (
	kw   = map[string]any{"type": "keyword"}
	long = map[string]any{"type": "long"}
	// textKeyword is searchable as text, and exact-matchable or aggregatable
	// through .keyword up to 1024 characters.
	textKeyword = map[string]any{"type": "text",
		"fields": map[string]any{"keyword": map[string]any{"type": "keyword", "ignore_above": 1024}}}
	// auditMapping is strict, like sre-triage's: a typo'd field fails loudly
	// at write time instead of quietly becoming a new column nobody queries.
	auditMapping = map[string]any{"dynamic": "strict", "properties": map[string]any{
		"@timestamp": map[string]any{"type": "date"},
		"action":     kw, "status": kw, "user": kw, "on_behalf_of": kw, "remote": kw, "ms": long,
		"reason": map[string]any{"type": "text"},
		"host":   kw, "asset": kw, "asset_name": kw, "account": kw, "protocol": kw, "ticket": kw, "gateway": kw,
		"sql": textKeyword, "command": textKeyword,
		"rows": long, "email": kw, "fingerprint": kw,
		"requested": kw, "target": kw, "bytes_up": long, "bytes_down": long, "streams": long,
	}}
	// auditSetup is PUT at startup and is idempotent, so this file -- not
	// whatever someone clicked in Kibana -- is the source of truth.
	auditSetup = []struct {
		path string
		body any
	}{
		{"/_ilm/policy/" + auditName + "-30d", map[string]any{"policy": map[string]any{"phases": map[string]any{
			"hot":    map[string]any{"min_age": "0ms", "actions": map[string]any{}},
			"delete": map[string]any{"min_age": "30d", "actions": map[string]any{"delete": map[string]any{}}},
		}}}},
		{"/_index_template/" + auditName, map[string]any{
			"index_patterns": []string{auditPrefix + "*"}, "priority": 200,
			"template": map[string]any{
				"settings": map[string]any{"number_of_shards": 1, "number_of_replicas": 0,
					"index.lifecycle.name": auditName + "-30d"},
				"mappings": auditMapping,
			}}},
		// New fields onto indices that already exist (today's, mid-deploy); a
		// no-op on first deploy, when there are none.
		{"/" + auditPrefix + "*/_mapping?allow_no_indices=true&ignore_unavailable=true", auditMapping},
	}
	// Beijing time, fixed rather than loaded: the release binary is static
	// and may have no tzdata, and the day boundary must match sre-triage's.
	cst = time.FixedZone("CST", 8*3600)
)

// auditShipper queues events and bulk-writes them. Nil-safe: a jmsd without
// audit_es configured holds a nil shipper and send does nothing.
type auditShipper struct {
	es   string
	host string
	q    chan auditEvent
	http *http.Client
}

func newAuditShipper(esURL, host string) *auditShipper {
	a := &auditShipper{
		es: strings.TrimRight(esURL, "/"), host: host,
		// ponytail: in-memory queue; events not yet flushed when jmsd dies are
		// lost. A disk spool if that ever matters.
		q:    make(chan auditEvent, 50_000),
		http: &http.Client{Timeout: 30 * time.Second},
	}
	go a.run()
	return a
}

// send never blocks. A full queue means ES has been unreachable for a long
// while; the event is then logged whole, so it still exists somewhere.
func (a *auditShipper) send(ev auditEvent) {
	if a == nil {
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	ev.Host = a.host
	select {
	case a.q <- ev:
	default:
		b, _ := json.Marshal(ev)
		log.Printf("jmsd: audit queue full, dropped %s", b)
	}
}

func (a *auditShipper) run() {
	for _, s := range auditSetup {
		if err := a.do(http.MethodPut, s.path, "application/json", mustJSON(s.body)); err != nil {
			// Writes still work if the template already exists.
			log.Printf("jmsd: audit setup %s: %v", s.path, err)
		}
	}
	for {
		batch := []auditEvent{<-a.q}
		deadline := time.After(auditWait)
	fill:
		for len(batch) < auditBatch {
			select {
			case ev := <-a.q:
				batch = append(batch, ev)
			case <-deadline:
				break fill
			}
		}
		a.flush(batch)
	}
}

func (a *auditShipper) flush(batch []auditEvent) {
	var body bytes.Buffer
	for _, ev := range batch {
		index := auditPrefix + ev.Timestamp.In(cst).Format("2006-01-02")
		body.Write(mustJSON(map[string]any{"index": map[string]any{"_index": index}}))
		body.WriteByte('\n')
		body.Write(mustJSON(ev))
		body.WriteByte('\n')
	}
	if err := a.do(http.MethodPost, "/_bulk", "application/x-ndjson", body.Bytes()); err != nil {
		// ponytail: no retry, same as sre-triage; the batch is logged so the
		// record survives in journald.
		log.Printf("jmsd: audit bulk of %d failed: %v; events: %s", len(batch), err, body.String())
	}
}

func (a *auditShipper) do(method, path, ctype string, body []byte) error {
	req, err := http.NewRequest(method, a.es+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, data)
	}
	// _bulk answers 200 even when every document was refused (a mapping
	// conflict, say); the per-item errors are the only sign.
	var out struct {
		Errors bool `json:"errors"`
		Items  []map[string]struct {
			Error json.RawMessage `json:"error"`
		} `json:"items"`
	}
	if json.Unmarshal(data, &out) == nil && out.Errors {
		for _, item := range out.Items {
			for _, r := range item {
				if len(r.Error) > 0 {
					return fmt.Errorf("documents rejected, first: %s", r.Error)
				}
			}
		}
	}
	return nil
}

// remoteIP drops the port: the port of a client's outbound socket is noise in
// an audit record, and it would make every event from one laptop distinct.
func remoteIP(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only ever called on values built in this file
	}
	return b
}
