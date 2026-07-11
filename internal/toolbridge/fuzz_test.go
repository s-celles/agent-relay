package toolbridge

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// FuzzHandleMCP throws arbitrary bytes at the MCP endpoint — the surface the
// CLI subprocess (an untrusted parser peer) speaks to — and asserts the
// handler never panics and always answers something sensible. A resolver
// goroutine drains parked tool calls so tools/call inputs cannot wedge the
// fuzz loop.
func FuzzHandleMCP(f *testing.F) {
	b, err := New(100 * time.Millisecond)
	if err != nil {
		f.Fatalf("New: %v", err)
	}
	defer b.Close()
	s := b.NewSession(weather)

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case call, ok := <-s.Calls():
				if !ok {
					return
				}
				_ = s.Resolve(call.ID, "ok", false)
			case <-done:
				return
			}
		}
	}()

	seeds := []string{
		``,
		`not json`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"Paris"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_weather","arguments":"not an object"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"garbage"}`,
		`{"jsonrpc":"2.0","id":1,"method":"no/such/method"}`,
		`{"jsonrpc":"2.0","id":{"weird":"id"},"method":"initialize"}`,
	}
	for _, sd := range seeds {
		f.Add([]byte(sd))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		req := httptest.NewRequest("POST", s.URL(), bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+s.Token())
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		b.srv.Handler.ServeHTTP(rec, req)

		switch rec.Code {
		case http.StatusOK, http.StatusAccepted, http.StatusBadRequest,
			http.StatusInternalServerError:
		default:
			t.Errorf("unexpected status %d for %q", rec.Code, data)
		}
	})
}
