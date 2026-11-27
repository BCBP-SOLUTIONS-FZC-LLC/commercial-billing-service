package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A callee's error is read through platform-gincommon's problem decoder
// (BIL-API-6): errorCode from an application/problem+json body, and from a
// legacy application/json body that still carries one.
func TestCallReadsProblemErrorCode(t *testing.T) {
	for _, c := range []struct{ name, ctype, body, want string }{
		{"problem", "application/problem+json", `{"type":"https://errors.bcbpsolutions.com/iam/realm-already-dedicated","title":"Realm already dedicated","status":409,"errorCode":"REALM_ALREADY_DEDICATED","traceId":"abc"}`, "REALM_ALREADY_DEDICATED"},
		{"legacy json", "application/json", `{"errorCode":"operation_in_progress"}`, "operation_in_progress"},
		{"not json", "text/plain", `upstream connect error`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var accept string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				accept = r.Header.Get("Accept")
				w.Header().Set("Content-Type", c.ctype)
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			r, err := Call(context.Background(), New("test", time.Second), http.MethodGet, srv.URL, "t1", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != http.StatusConflict || r.ErrorCode != c.want {
				t.Fatalf("got %d %q, want 409 %q", r.Status, r.ErrorCode, c.want)
			}
			if accept != "application/json, application/problem+json" {
				t.Errorf("Accept = %q", accept)
			}
		})
	}
}
