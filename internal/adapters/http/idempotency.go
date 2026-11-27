package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// idemMode is a route's Idempotency-Key rule (§5.1, §5.3).
type idemMode int

const (
	idemNone idemMode = iota
	idemOptional
	idemRequired
)

const (
	headerIdempotencyKey = "Idempotency-Key"
	headerReplayed       = "Idempotent-Replayed"
	// ctxKeepInFlight is set by a handler whose outcome must not be stored:
	// the record stays in_flight and unlocked, so a retry with the same key
	// runs again (B-I1 PAYMENT_ACTION_REQUIRED, §5.4.2 step 5).
	ctxKeepInFlight = "billing.idempotency.keep_in_flight"
)

// idempotencyKeyPattern is §5.1: 8–200 of [A-Za-z0-9:_\-.].
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9:_\-.]{8,200}$`)

// Idempotency is the §9.2 middleware on idempotency_records.
type Idempotency struct {
	Repo port.IdempotencyRepo
	Tx   port.TxRunner
	TTL  time.Duration // IDEMPOTENCY_TTL_HOURS
	Lock time.Duration // in-flight lock (30 s)
	Now  func() time.Time
}

// idemRoute describes one route for the middleware.
type idemRoute struct {
	scope  string                                         // route id
	mode   idemMode                                       // none | optional | required
	tenant func(c *gin.Context) string                    // whose records (RLS)
	key    func(c *gin.Context, tenant, key string) error // extra key rule (B-I1)
	reused func(stored []byte) error                      // body mismatch (default 422 IDEMPOTENCY_KEY_REUSED)
}

// handle returns the middleware for r.
func (m *Idempotency) handle(r idemRoute) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader(headerIdempotencyKey)
		if r.mode == idemNone || (key == "" && r.mode == idemOptional) {
			c.Next()
			return
		}
		if key == "" {
			writeError(c, validationError(headerIdempotencyKey, "is required on this route"))
			return
		}
		if !idempotencyKeyPattern.MatchString(key) {
			writeError(c, validationError(headerIdempotencyKey, "must be 8–200 characters of [A-Za-z0-9:_-.]"))
			return
		}
		tenant := r.tenant(c)
		if r.key != nil {
			if err := r.key(c, tenant, key); err != nil {
				writeError(c, err)
				return
			}
		}
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			writeError(c, err)
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		hash := requestHash(c, body)

		now := m.now()
		lockUntil := now.Add(m.Lock)
		rec := domain.IdempotencyRecord{TenantID: tenant, Scope: r.scope, Key: key, RequestHash: hash,
			LockedUntil: &lockUntil, ExpiresAt: now.Add(m.TTL)}
		ctx := c.Request.Context()
		var existing *domain.IdempotencyRecord
		if err := m.Tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
			var err error
			existing, err = m.Repo.Begin(ctx, rec)
			if err != nil || existing == nil {
				return err
			}
			// Expired, or an abandoned in-flight run of the same request:
			// this request takes it over.
			if existing.ExpiresAt.Before(now) || (existing.State == domain.IdempotencyInFlight && bytes.Equal(existing.RequestHash, hash) &&
				(existing.LockedUntil == nil || existing.LockedUntil.Before(now))) {
				ok, err := m.Repo.Reclaim(ctx, rec)
				if err == nil && ok {
					existing = nil
				}
				return err
			}
			return nil
		}); err != nil {
			writeError(c, err)
			return
		}
		if existing != nil {
			m.answerExisting(c, r, existing, hash)
			return
		}

		cw := &captureWriter{ResponseWriter: c.Writer}
		c.Writer = cw
		c.Next()
		m.finish(c, r, tenant, key, cw)
	}
}

// answerExisting answers a request whose key is already recorded.
func (m *Idempotency) answerExisting(c *gin.Context, r idemRoute, e *domain.IdempotencyRecord, hash []byte) {
	if !bytes.Equal(e.RequestHash, hash) {
		if r.reused != nil {
			writeError(c, r.reused(e.ResponseBody))
			return
		}
		writeError(c, domain.NewError(domain.ErrIdempotencyKeyReused, "Idempotency-Key was used with a different request"))
		return
	}
	if e.State != domain.IdempotencyCompleted || e.StatusCode == nil {
		writeError(c, domain.NewError(domain.ErrRequestInProgress, "a request with this Idempotency-Key is in progress"))
		return
	}
	status := *e.StatusCode
	if status == http.StatusCreated {
		status = http.StatusOK // §5.5: replays are 200
	}
	c.Header(headerReplayed, "true")
	if len(e.ResponseBody) == 0 || string(e.ResponseBody) == "null" {
		c.Status(status)
		c.Abort()
		return
	}
	ct := "application/json; charset=utf-8"
	if *e.StatusCode >= 400 {
		ct = "application/problem+json"
	}
	c.Data(status, ct, e.ResponseBody)
	c.Abort()
}

// finish stores the outcome, or releases the key when it must not be
// replayed: validation failures (the client fixes and retries), server and
// dependency failures, and in-progress answers.
func (m *Idempotency) finish(c *gin.Context, r idemRoute, tenant, key string, cw *captureWriter) {
	ctx := context.WithoutCancel(c.Request.Context())
	status := cw.Status()
	var err error
	switch {
	case c.GetBool(ctxKeepInFlight):
		err = m.Tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error { return m.Repo.Unlock(ctx, tenant, r.scope, key) })
	case status >= 500 || status == http.StatusBadRequest || status == http.StatusConflict && isInProgress(cw.buf.Bytes()):
		err = m.Tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error { return m.Repo.Release(ctx, tenant, r.scope, key) })
	default:
		body := cw.buf.Bytes()
		if !json.Valid(body) {
			body = nil
		}
		err = m.Tx.RunInTenantTx(ctx, tenant, func(ctx context.Context) error {
			return m.Repo.Complete(ctx, tenant, r.scope, key, status, body)
		})
	}
	if err != nil {
		_ = c.Error(err) // logged by gincommon; the response is already sent
	}
}

func isInProgress(body []byte) bool {
	var p struct {
		ErrorCode string `json:"errorCode"`
	}
	_ = json.Unmarshal(body, &p)
	return p.ErrorCode == string(domain.ErrRequestInProgress)
}

// requestHash is SHA-256 of the method, path and compact JSON body (§4.2.19).
func requestHash(c *gin.Context, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(c.Request.Method + " " + c.Request.URL.Path + "\n"))
	var compact bytes.Buffer
	if json.Compact(&compact, body) == nil {
		h.Write(compact.Bytes())
	} else {
		h.Write(body)
	}
	return h.Sum(nil)
}

func (m *Idempotency) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// captureWriter keeps a copy of the response for the idempotency record.
type captureWriter struct {
	gin.ResponseWriter
	buf bytes.Buffer
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	w.buf.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// keepInFlight marks the response as not to be stored (see ctxKeepInFlight).
func keepInFlight(c *gin.Context) { c.Set(ctxKeepInFlight, true) }
