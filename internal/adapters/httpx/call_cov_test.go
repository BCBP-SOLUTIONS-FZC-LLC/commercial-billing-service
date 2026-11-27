package httpx

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCallRequestBuildErrors(t *testing.T) {
	c := New("test", time.Second)
	// A body that cannot be JSON-encoded.
	if _, err := Call(context.Background(), c, http.MethodPost, "http://127.0.0.1:1/x", "t1", "", map[string]any{"f": func() {}}); err == nil {
		t.Error("unencodable body accepted")
	}
	// A malformed URL.
	if _, err := Call(context.Background(), c, http.MethodGet, "http://[::1", "t1", "", nil); err == nil {
		t.Error("malformed URL accepted")
	}
}

// A body cut short by the callee is a transport failure (status 0).
func TestCallTruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\n{\"a\":")
		_ = buf.Flush()
		_ = conn.(*net.TCPConn).CloseWrite()
		_ = conn.Close()
	}))
	defer srv.Close()
	r, err := Call(context.Background(), New("test", time.Second), http.MethodGet, srv.URL, "t1", "", nil)
	if err == nil || r.Status != 0 {
		t.Fatalf("truncated body: %+v %v", r, err)
	}
}

func TestResponseDecodeError(t *testing.T) {
	var v struct{ A int }
	err := Response{Status: 200, Body: []byte("nope")}.Decode(&v)
	if err == nil || !strings.Contains(err.Error(), "decode 200 response") {
		t.Errorf("decode: %v", err)
	}
}

func TestOperationEdgeCases(t *testing.T) {
	for _, c := range []struct{ method, path, want string }{
		{http.MethodGet, "/v1/invoices/42", "invoices.retrieve"},
		{"OPTIONS", "/v1/customers", "customers.options"},
		{http.MethodGet, "/" + strings.Repeat("segment/", 12), strings.Repeat("segment.", 8)[:64]},
	} {
		if got := Operation(c.method, c.path); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}
