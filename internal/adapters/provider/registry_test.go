package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/adapters/provider/manual"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

// fakeProvider is a loaded adapter with a chosen name and version.
type fakeProvider struct {
	port.PaymentProvider // nil: only Name and AdapterVersion are used here
	name, version        string
}

func (f fakeProvider) Name() string           { return f.name }
func (f fakeProvider) AdapterVersion() string { return f.version }

type fakeCerts struct {
	valid map[string]bool // key provider|version|suite
	err   error
}

func (f *fakeCerts) Enabling(_ context.Context, p, v, s string) (domain.Certification, bool, error) {
	if f.err != nil {
		return domain.Certification{}, false, f.err
	}
	return domain.Certification{}, f.valid[p+"|"+v+"|"+s], nil
}

func code(err error) domain.ErrorCode {
	var de *domain.DomainError
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func TestUnknownListedProviderFailsStartup(t *testing.T) {
	_, err := New([]port.PaymentProvider{manual.New()}, []string{"manual", "adyen"}, &fakeCerts{}, "cert-1.0", nil)
	if err == nil {
		t.Fatal("a listed provider without a compiled adapter must fail startup (§12)")
	}
}

// BIL-PROV-1 and BIL-PROV-2.
func TestBIL_PROV_1_and_2_Gate(t *testing.T) {
	stripe := fakeProvider{name: "stripe", version: "1.0.0+stripeapi.2026-10-28.acacia"}
	certs := &fakeCerts{valid: map[string]bool{"manual|1.0.0+manual|cert-1.0": true}}
	r, err := New([]port.PaymentProvider{manual.New(), stripe}, []string{"stripe", "manual"}, certs, "cert-1.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := r.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 || st[0].Provider != "manual" || !st[0].Enabled || st[1].Provider != "stripe" || st[1].Enabled {
		t.Fatalf("statuses %+v", st)
	}
	// Uncertified stripe: no new accounts, but still loaded for running work.
	if _, err := r.ForNewAccount("stripe"); code(err) != domain.ErrProviderNotEnabled {
		t.Fatalf("uncertified stripe for a new account: %v", err)
	}
	if p, ok := r.Loaded("stripe"); !ok || p.Name() != "stripe" {
		t.Fatal("uncertified stripe must stay loaded (BIL-PROV-2)")
	}
	if _, err := r.ForNewAccount("manual"); err != nil {
		t.Fatalf("exempt manual: %v", err)
	}
	// An approval takes effect on the next refresh, without a restart.
	certs.valid["stripe|1.0.0+stripeapi.2026-10-28.acacia|cert-1.0"] = true
	if _, err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ForNewAccount("stripe"); err != nil {
		t.Fatalf("approved stripe: %v", err)
	}
	// A revocation disables new accounts again.
	delete(certs.valid, "stripe|1.0.0+stripeapi.2026-10-28.acacia|cert-1.0")
	_, _ = r.Refresh(ctx)
	if _, err := r.ForNewAccount("stripe"); code(err) != domain.ErrProviderNotEnabled {
		t.Fatal("revoked stripe must stop taking new accounts")
	}
	// A read failure keeps the previous state.
	certs.valid["stripe|1.0.0+stripeapi.2026-10-28.acacia|cert-1.0"] = true
	_, _ = r.Refresh(ctx)
	certs.err = errors.New("db down")
	if _, err := r.Refresh(ctx); err == nil {
		t.Fatal("refresh must report the read failure")
	}
	if _, err := r.ForNewAccount("stripe"); err != nil {
		t.Fatal("a failed refresh must not disable a certified provider")
	}
}

// A different suite version or adapter version does not enable (BIL-PROV-2).
func TestBIL_PROV_2_VersionMustMatchExactly(t *testing.T) {
	stripe := fakeProvider{name: "stripe", version: "1.0.1+stripeapi.2026-11-15.acacia"}
	certs := &fakeCerts{valid: map[string]bool{"stripe|1.0.0+stripeapi.2026-10-28.acacia|cert-1.0": true}}
	r, _ := New([]port.PaymentProvider{stripe}, []string{"stripe"}, certs, "cert-1.0", nil)
	_, _ = r.Refresh(context.Background())
	if _, err := r.ForNewAccount("stripe"); code(err) != domain.ErrProviderNotEnabled {
		t.Fatal("a new adapter version needs its own certification")
	}
}

func TestNotLoadedProvider(t *testing.T) {
	r, _ := New([]port.PaymentProvider{manual.New()}, []string{"manual"}, &fakeCerts{}, "cert-1.0", nil)
	if _, ok := r.Loaded("stripe"); ok {
		t.Fatal("stripe is not listed, so not loaded")
	}
	if _, err := r.ForNewAccount("stripe"); code(err) != domain.ErrProviderNotEnabled {
		t.Fatal("not loaded → PROVIDER_NOT_ENABLED")
	}
}
