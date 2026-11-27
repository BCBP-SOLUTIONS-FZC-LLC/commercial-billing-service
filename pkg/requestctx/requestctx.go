// Package requestctx carries the caller identity AuthZ Enrichment injects
// (x-user-id, x-tenant-id, x-tenant-roles) plus client metadata through a
// request, and binds app.tenant_id for RLS (LLD §3, §4.3, §5.1).
package requestctx

import "context"

// RequestContext is the per-request caller identity.
type RequestContext struct {
	UserID    string
	TenantID  string
	Roles     []string
	ClientIP  string
	UserAgent string
}

type ctxKey struct{}

// With returns ctx carrying rc.
func With(ctx context.Context, rc RequestContext) context.Context {
	return context.WithValue(ctx, ctxKey{}, rc)
}

// From returns the RequestContext in ctx, if any.
func From(ctx context.Context) (RequestContext, bool) {
	rc, ok := ctx.Value(ctxKey{}).(RequestContext)
	return rc, ok
}

// HasRole reports whether the caller holds role.
func (rc RequestContext) HasRole(role string) bool {
	for _, r := range rc.Roles {
		if r == role {
			return true
		}
	}
	return false
}
