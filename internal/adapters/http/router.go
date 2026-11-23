// Package http is Billing's Gin adapter: router, handlers, DTOs and error
// mapping (LLD §5). Phase 0 wires only the infrastructure probes; the tenant,
// internal, webhook and operator groups are added in Phase 5.
package http

import (
	"context"
	"net/http"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/gin-gonic/gin"
)

// defaultRequestTimeout is the gincommon request deadline when the config
// does not set one (REQUEST_TIMEOUT, §12).
const defaultRequestTimeout = 30 * time.Second

// maxBodyBytes caps JSON request bodies. The webhook route sets its own limit
// from WEBHOOK_MAX_BODY_BYTES (§8.15).
const maxBodyBytes = 1 << 20

// Pinger is any dependency a probe checks.
type Pinger interface {
	Health(ctx context.Context) error
}

// PingerFunc adapts a function to Pinger.
type PingerFunc func(ctx context.Context) error

// Health calls f.
func (f PingerFunc) Health(ctx context.Context) error { return f(ctx) }

// Check is one named readiness dependency.
type Check struct {
	Name   string
	Pinger Pinger
}

// RouterConfig carries what the router needs.
type RouterConfig struct {
	// GinConfig is gincommon's identity and observability config. When
	// Logger is nil (unit tests) the observability middlewares are skipped.
	GinConfig gincommon.Config
	// Readiness are the dependencies /readyz requires: the app and system
	// pools now; the outbox runner, webhook processor and SQS consumers as
	// later phases add them. Any failure answers 503.
	Readiness []Check
	// Liveness are background loops /healthz checks (for example the
	// platform-audit runner, which belongs in liveness, never readiness).
	Liveness []Check
	// Routes mount further route groups (B-W1 now; tenant, internal and
	// operator groups in Phase 5). They run after the observability
	// middlewares.
	Routes []func(gin.IRouter)
}

// Router is the HTTP handler for the API listener (APP_PORT). Metrics are
// served on a separate listener (METRICS_PORT) by cmd/server.
type Router struct {
	engine *gin.Engine
}

// Handler returns the http.Handler to serve.
func (r *Router) Handler() http.Handler { return r.engine }

// NewRouter builds the router.
func NewRouter(cfg RouterConfig) *Router {
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		c.Next()
	})
	if cfg.GinConfig.Logger != nil {
		ginCfg := cfg.GinConfig
		if ginCfg.RequestTimeout <= 0 {
			ginCfg.RequestTimeout = defaultRequestTimeout
		}
		// Panic recovery → request ID → tracing → correlation → metrics →
		// logging → request deadline (innermost), §3.3.
		r.Use(gincommon.ObservabilityMiddlewares(ginCfg)...)
	} else {
		r.Use(gin.Recovery())
	}

	h := &probes{readiness: cfg.Readiness, liveness: cfg.Liveness}
	r.GET("/healthz", h.healthz)
	r.GET("/readyz", h.readyz)
	for _, mount := range cfg.Routes {
		mount(r)
	}
	return &Router{engine: r}
}

type probes struct {
	readiness []Check
	liveness  []Check
}

// probeTimeout bounds every dependency check, so a hung dependency cannot
// hang the probe (pgcommon Pool.Health requires a deadline).
const probeTimeout = 5 * time.Second

// healthz is the liveness probe: 200 while the process answers and its
// background loops are alive. It never checks dependencies.
func (p *probes) healthz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), probeTimeout)
	defer cancel()
	for _, chk := range p.liveness {
		if chk.Pinger == nil {
			continue
		}
		if err := chk.Pinger.Health(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy", "failed": chk.Name})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz is the readiness probe: 503 when any required dependency fails.
// Error text is not returned, so the probe never leaks connection details.
func (p *probes) readyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), probeTimeout)
	defer cancel()
	checks := gin.H{}
	ready := true
	for _, chk := range p.readiness {
		if chk.Pinger == nil {
			continue
		}
		if err := chk.Pinger.Health(ctx); err != nil {
			checks[chk.Name] = "down"
			ready = false
			continue
		}
		checks[chk.Name] = "ok"
	}
	status, overall := http.StatusOK, "ready"
	if !ready {
		status, overall = http.StatusServiceUnavailable, "not ready"
	}
	c.JSON(status, gin.H{"status": overall, "checks": checks})
}
