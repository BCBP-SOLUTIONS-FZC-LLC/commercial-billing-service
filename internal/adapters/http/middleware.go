package http

import (
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
)

// apiBodyLimit is §10.4's JSON body limit for API routes.
const apiBodyLimit = 64 << 10

const (
	headerTenantID = "x-tenant-id"
	headerXFCC     = "X-Forwarded-Client-Cert"
	ctxPrincipal   = "billing.principal"
)

// caller builds the request identity from gincommon's verified request
// context (x-user-id, x-tenant-id, x-tenant-roles) and, on the internal
// route, the mTLS principal. Nothing is read from the body (BIL-API-1).
func caller(c *gin.Context) app.Caller {
	out := app.Caller{ClientIP: c.ClientIP(), UserAgent: c.Request.UserAgent(), Principal: c.GetString(ctxPrincipal)}
	if rc, ok := gincommon.RequestContext(c); ok && rc != nil {
		out.UserID, out.TenantID, out.Roles = rc.UserID, rc.TenantID, rc.Roles
	}
	if out.TenantID == "" {
		out.TenantID = strings.TrimSpace(c.GetHeader(headerTenantID))
	}
	return out
}

// guard turns an authorization check into a middleware.
func guard(check func(c *gin.Context, who app.Caller) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := check(c, caller(c)); err != nil {
			writeError(c, err)
			return
		}
		c.Next()
	}
}

// rejectSystemCaller is BIL-AUTH-4.
func rejectSystemCaller() gin.HandlerFunc {
	return guard(func(_ *gin.Context, who app.Caller) error { return app.RejectSystemCaller(who) })
}

// requirePathTenant is BIL-API-2, before any query.
func requirePathTenant() gin.HandlerFunc {
	return guard(func(c *gin.Context, who app.Caller) error { return app.RequireTenantMatch(who, c.Param("id")) })
}

// requirePermission is BIL-AUTH-1: the route's one permission (§5.3).
func requirePermission(route string) gin.HandlerFunc {
	p, ok := domain.TenantRoutePermission[route]
	if !ok {
		panic("no permission declared for tenant route " + route)
	}
	return guard(func(_ *gin.Context, who app.Caller) error { return app.RequirePermission(who, p) })
}

// requireOperator is BIL-AUTH-2 and the BIL-AUTH-6 gate.
func requireOperator(live, devLike bool) gin.HandlerFunc {
	return guard(func(_ *gin.Context, who app.Caller) error { return app.RequirePlatformOperator(who, live, devLike) })
}

// requirePCIReviewer is B-O17's extra role (BIL-PROV-4).
func requirePCIReviewer() gin.HandlerFunc {
	return guard(func(_ *gin.Context, who app.Caller) error { return app.RequirePCIReviewer(who) })
}

// requireBFFCaller is BIL-AUTH-3. The mesh sidecar forwards the verified
// client certificate in X-Forwarded-Client-Cert; the principal is the
// SPIFFE URI of the nearest hop (the last element).
func requireBFFCaller(allowed string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(ctxPrincipal, principalFromXFCC(c.GetHeader(headerXFCC)))
		if err := app.RequireBFFCaller(caller(c), allowed); err != nil {
			writeError(c, err)
			return
		}
		c.Next()
	}
}

// principalFromXFCC extracts "cluster.local/ns/<ns>/sa/<sa>" from the last
// XFCC element's URI=spiffe://… pair.
func principalFromXFCC(h string) string {
	if h == "" {
		return ""
	}
	elems := strings.Split(h, ",")
	for _, pair := range strings.Split(elems[len(elems)-1], ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && strings.EqualFold(k, "URI") {
			return strings.TrimPrefix(strings.Trim(v, `"`), "spiffe://")
		}
	}
	return ""
}

// requireJSONContentType answers 415 UNSUPPORTED_MEDIA_TYPE for a request
// with a body that is not application/json (§5.1).
func requireJSONContentType() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength == 0 || c.Request.Method == http.MethodGet {
			c.Next()
			return
		}
		mt, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || mt != "application/json" {
			gincommon.AbortWithProblem(c, problem.NewError(problem.TypeUnsupportedMediaType, "request bodies must be application/json").
				WithCode(string(domain.ErrUnsupportedMediaType)))
			return
		}
		c.Next()
	}
}

// bodyLimit caps the request body (§10.4).
func bodyLimit(n int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)
		c.Next()
	}
}
