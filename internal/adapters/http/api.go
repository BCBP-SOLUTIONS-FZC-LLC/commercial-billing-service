package http

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// APIConfig wires the tenant, internal and operator route groups (§5).
type APIConfig struct {
	GinConfig gincommon.Config // for ProtectedMiddlewares (logger may be nil)
	Tenant    app.TenantAPI
	Internal  app.InternalAPI
	Operator  app.OperatorAPI
	Idem      *Idempotency
	// OperatorPathLive and DevLike drive the BIL-AUTH-6 gate.
	OperatorPathLive bool
	DevLike          bool
	// BFFCallerPrincipal is BIL-AUTH-3's allowed mTLS principal.
	BFFCallerPrincipal string
	// Disabled lists routes behind a feature flag that is off: they are not
	// registered, so they answer 404 (§12 feature flags).
	Disabled map[string]bool
	// OnOperatorAction counts commercial_billing_operator_actions_total{action,
	// outcome}: success < 400, failure 4xx, error 5xx (§11.2). Optional.
	OnOperatorAction func(action, outcome string)
}

// Route is one endpoint of the catalogue (§5.3).
type Route struct {
	ID     string
	Method string
	Path   string
	Idem   idemMode
}

// Routes returns the catalogue in registration order: 26 tenant routes,
// B-I1 and 18 operator routes (B-W1 is mounted by the webhook adapter).
func Routes() []Route {
	out := make([]Route, 0, 45)
	for _, r := range tenantRoutes {
		out = append(out, r.Route)
	}
	out = append(out, internalRoute.Route)
	for _, r := range operatorRoutes {
		out = append(out, r.Route)
	}
	return out
}

// handlerFactory builds a route's handler and records its request and
// response types and success status, from which the OpenAPI document is
// generated (openapi.go), so the document cannot drift from the code.
type handlerFactory struct {
	make   func(a *APIConfig) gin.HandlerFunc
	req    reflect.Type // nil: no body
	res    reflect.Type // nil: no body
	status int
}

type routeDef struct {
	Route
	handler handlerFactory
	extra   []gin.HandlerFunc // route-specific guards (B-O17)
}

func typeOf[T any]() reflect.Type { return reflect.TypeFor[T]() }

const (
	tenantPrefix   = "/api/v1/tenants/:id"
	operatorPrefix = "/api/v1/operator"
)

// Register mounts the three groups on r.
func (a *APIConfig) Register(r gin.IRouter) {
	// Tenant: identity → BIL-AUTH-4 → 415 → BIL-API-2 → per route
	// permission (BIL-AUTH-1) → idempotency → handler. RLS is bound per
	// transaction by the TxRunner (GUCBridge's job, §3.3).
	tenant := r.Group(tenantPrefix, append(gincommon.ProtectedMiddlewares(a.GinConfig),
		rejectSystemCaller(), bodyLimit(apiBodyLimit), requireJSONContentType(), requirePathTenant())...)
	for _, d := range tenantRoutes {
		if a.Disabled[d.ID] {
			continue
		}
		chain := []gin.HandlerFunc{requirePermission(d.ID), a.Idem.handle(idemRoute{scope: d.ID, mode: d.Idem, tenant: pathTenant})}
		tenant.Handle(d.Method, d.Path[len(tenantPrefix):], append(chain, d.handler.make(a))...)
	}

	// Internal (B-I1): mTLS principal only; no gateway identity.
	if !a.Disabled[internalRoute.ID] {
		a.registerInternal(r)
	}

	// Operator: identity → BIL-AUTH-4 → BIL-AUTH-2/6 → 415 → per route.
	a.registerOperator(r)
}

func (a *APIConfig) registerInternal(r gin.IRouter) {
	r.Handle(internalRoute.Method, internalRoute.Path, requireBFFCaller(a.BFFCallerPrincipal), rejectSystemCaller(),
		bodyLimit(apiBodyLimit), requireJSONContentType(),
		a.Idem.handle(idemRoute{scope: "B-I1", mode: idemRequired, tenant: headerTenant, key: chargeKey, reused: subscriptionExists}),
		internalRoute.handler.make(a))
}

func (a *APIConfig) registerOperator(r gin.IRouter) {
	ids := map[string]string{}
	for _, d := range operatorRoutes {
		ids[d.Method+" "+d.Path] = d.ID
	}
	op := r.Group(operatorPrefix, append(append([]gin.HandlerFunc{countOperatorAction(ids, a.OnOperatorAction)}, gincommon.ProtectedMiddlewares(a.GinConfig)...),
		rejectSystemCaller(), requireOperator(a.OperatorPathLive, a.DevLike), bodyLimit(apiBodyLimit), requireJSONContentType())...)
	for _, d := range operatorRoutes {
		if a.Disabled[d.ID] {
			continue
		}
		chain := append([]gin.HandlerFunc{}, d.extra...)
		chain = append(chain, a.Idem.handle(idemRoute{scope: d.ID, mode: d.Idem, tenant: operatorTenant}))
		op.Handle(d.Method, d.Path[len(operatorPrefix):], append(chain, d.handler.make(a))...)
	}
}

// pathTenant is the tenant whose records a tenant route writes.
func pathTenant(c *gin.Context) string { return c.Param("id") }

// headerTenant is B-I1's pre-allocated tenant (x-tenant-id).
func headerTenant(c *gin.Context) string { return c.GetHeader(headerTenantID) }

// operatorTenant is the path tenant, or platform_tenant for the global
// operator routes (/api/v1/operator/billing/…).
func operatorTenant(c *gin.Context) string {
	if id := c.Param("id"); id != "" && strings.HasPrefix(c.Request.URL.Path, operatorPrefix+"/tenants/") {
		return id
	}
	return app.PlatformTenantID
}

// chargeKey is B-I1's rule: Idempotency-Key = charge:<tenant_id> (SB-INV-3).
func chargeKey(_ *gin.Context, tenant, key string) error {
	if tenant == "" || key != "charge:"+tenant {
		return validationError(headerIdempotencyKey, "must be charge:<tenant_id>")
	}
	return nil
}

// subscriptionExists is B-I1's answer to a key reused with another body
// (§9.2): 409 SUBSCRIPTION_ALREADY_EXISTS with the stored subscription id.
func subscriptionExists(stored []byte) error {
	err := domain.NewError(domain.ErrSubscriptionAlreadyExists, "a subscription was already requested for this tenant")
	var body struct {
		ID string `json:"billing_subscription_id"`
	}
	if json.Unmarshal(stored, &body) == nil && body.ID != "" {
		return err.With("billing_subscription_id", body.ID)
	}
	return err
}

// ── Handler helpers ────────────────────────────────────────────────────────

// withBody decodes and validates the body, then calls fn and writes its
// result with status.
func withBody[Req, Res any](status int, strict bool, fn func(a *APIConfig, c *gin.Context, who app.Caller, req Req) (Res, error)) handlerFactory {
	return handlerFactory{req: typeOf[Req](), res: typeOf[Res](), status: status, make: func(a *APIConfig) gin.HandlerFunc {
		return func(c *gin.Context) {
			var req Req
			if err := decodeJSON(c, &req, strict); err != nil {
				writeError(c, err)
				return
			}
			res, err := fn(a, c, caller(c), req)
			if err != nil {
				writeError(c, err)
				return
			}
			c.JSON(status, res)
		}
	}}
}

// noBody calls fn for a route without a request body.
func noBody[Res any](status int, fn func(a *APIConfig, c *gin.Context, who app.Caller) (Res, error)) handlerFactory {
	return handlerFactory{res: typeOf[Res](), status: status, make: func(a *APIConfig) gin.HandlerFunc {
		return func(c *gin.Context) {
			res, err := fn(a, c, caller(c))
			if err != nil {
				writeError(c, err)
				return
			}
			c.JSON(status, res)
		}
	}}
}

// noContent calls fn and answers 204.
func noContent(fn func(a *APIConfig, c *gin.Context, who app.Caller) error) handlerFactory {
	return handlerFactory{status: http.StatusNoContent, make: func(a *APIConfig) gin.HandlerFunc {
		return func(c *gin.Context) {
			if err := fn(a, c, caller(c)); err != nil {
				writeError(c, err)
				return
			}
			c.Status(http.StatusNoContent)
		}
	}}
}

// param returns a UUID path parameter or the validation error.
func param(c *gin.Context, name string) (string, error) { return pathUUID(c, name) }

// countOperatorAction reports every operator call, refused ones included,
// by route id once the response is written.
func countOperatorAction(ids map[string]string, on func(action, outcome string)) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if on == nil {
			return
		}
		action, ok := ids[c.Request.Method+" "+c.FullPath()]
		if !ok {
			action = "unknown"
		}
		switch st := c.Writer.Status(); {
		case st >= 500:
			on(action, "error")
		case st >= 400:
			on(action, "failure")
		default:
			on(action, "success")
		}
	}
}
