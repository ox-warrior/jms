package cli

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A service key must not touch the CDN at all: in a pinned image a forced
// update either fails every launch or replaces the binary the image shipped.
func TestServiceKeySkipsUpdateCheck(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if ch := updateCheckFor("/path/to/key", srv.URL); ch != nil {
		t.Fatal("-i must not start an update check")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("-i made %d requests to the update URL", n)
	}

	ch := updateCheckFor("", srv.URL)
	if ch == nil {
		t.Fatal("a person's launch must still check for updates")
	}
	<-ch
	if hits.Load() == 0 {
		t.Fatal("the check for a person never reached the update URL")
	}
}
