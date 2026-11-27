package requestctx

import (
	"context"
	"testing"
)

func TestWithFromAndHasRole(t *testing.T) {
	if _, ok := From(context.Background()); ok {
		t.Fatal("empty context carries a RequestContext")
	}
	ctx := With(context.Background(), RequestContext{UserID: "u1", TenantID: "t1", Roles: []string{"tenant_admin", "tenant_owner"}})
	rc, ok := From(ctx)
	if !ok || rc.UserID != "u1" || rc.TenantID != "t1" {
		t.Fatalf("From = %+v %v", rc, ok)
	}
	if !rc.HasRole("tenant_owner") || rc.HasRole("member") {
		t.Error("HasRole wrong")
	}
}
