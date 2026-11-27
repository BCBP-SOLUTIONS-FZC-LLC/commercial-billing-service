package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/pkg/requestctx"
)

// Billing's outbound identity (BIL-AUTH-4, CD-19): IAM maps the reserved
// principal string to its sentinel UUID.
const (
	SystemPrincipal = "commercial-system"
	SystemRole      = "commercial-system"
	// HeaderRequestID carries the correlation ID to the callee.
	HeaderRequestID = "X-Request-ID"
)

// maxBody bounds a dependency's response body.
const maxBody = 1 << 20

// Response is a dependency's answer: status (0 = transport error or
// timeout) and the RFC 9457 errorCode when the body is a problem.
type Response struct {
	Status    int
	ErrorCode string
	Body      []byte
}

// OK reports a 2xx status.
func (r Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Call sends one request as commercial-system for tenantID. body is
// JSON-encoded when non-nil; idemKey sets Idempotency-Key when non-empty.
// A transport error or timeout returns Status 0 and the error.
func Call(ctx context.Context, c *http.Client, method, url, tenantID, idemKey string, body any) (Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Response{}, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Accept", "application/json, application/problem+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-user-id", SystemPrincipal)
	req.Header.Set("x-tenant-id", tenantID)
	req.Header.Set("x-tenant-roles", SystemRole)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	// The inbound request's correlation ID, when the call serves one
	// (background workers have none).
	if id := requestctx.RequestID(ctx); id != "" {
		req.Header.Set(HeaderRequestID, id)
	}
	resp, err := c.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Response{Status: 0}, err
	}
	out := Response{Status: resp.StatusCode, Body: b}
	if !out.OK() {
		// The callee's RFC 9457 problem, parsed by platform-gincommon (BIL-API-6):
		// branch on errorCode, never on title or detail. Decoded whatever the
		// Content-Type, so a callee still answering application/json with an
		// errorCode is read the same way.
		if p, err := problem.Decode(bytes.NewReader(b)); err == nil {
			out.ErrorCode = p.ErrorCode
		}
	}
	return out, nil
}

// Decode unmarshals a 2xx body into dst.
func (r Response) Decode(dst any) error {
	if err := json.Unmarshal(r.Body, dst); err != nil {
		return fmt.Errorf("decode %d response: %w", r.Status, err)
	}
	return nil
}

// ErrUnexpected marks a status the client does not expect.
var ErrUnexpected = errors.New("unexpected response")
