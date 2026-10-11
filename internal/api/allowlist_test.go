package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

func newTestServer(allowed []string) *Server {
	return &Server{smtpAPISecret: "s3cret", allowed: parseAllowedIPs(allowed), logger: zerolog.Nop()}
}

func doReq(s *Server, remote, secret string) int {
	h := s.allowListMiddleware(s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodGet, "/api/queue", nil)
	req.RemoteAddr = remote
	if secret != "" {
		req.Header.Set("X-API-Secret", secret)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestAllowListAndSecret(t *testing.T) {
	s := newTestServer([]string{"85.95.224.2", "92.42.33.0/24"})
	cases := []struct {
		remote, secret string
		want           int
	}{
		{"85.95.224.2:51000", "s3cret", 200},
		{"92.42.33.13:4000", "s3cret", 200},
		{"127.0.0.1:4000", "s3cret", 200}, // loopback always allowed
		{"[::1]:4000", "s3cret", 200},
		{"203.0.113.9:4000", "s3cret", 403}, // not in the list
		{"85.95.224.2:51000", "wrong", 401},
		{"85.95.224.2:51000", "", 401},
		{"203.0.113.9:4000", "", 403}, // allow-list answers before auth
	}
	for _, c := range cases {
		if got := doReq(s, c.remote, c.secret); got != c.want {
			t.Errorf("%s secret=%q: got %d want %d", c.remote, c.secret, got, c.want)
		}
	}
	open := newTestServer(nil) // older installs: no list = any source
	if got := doReq(open, "203.0.113.9:4000", "s3cret"); got != 200 {
		t.Errorf("empty allow-list must not restrict, got %d", got)
	}
}
