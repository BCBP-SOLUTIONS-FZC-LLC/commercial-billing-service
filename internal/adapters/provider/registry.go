// Package provider holds the payment-provider registry and the certification
// gate (LLD §3.5).
//
// Two sets:
//   - loaded: every compiled adapter listed in ENABLED_PAYMENT_PROVIDERS.
//     Existing accounts keep working on a loaded provider even when its
//     certification lapses ("fail open for running work", BIL-PROV-2).
//   - enabled for new accounts: loaded providers with a passed (or, for
//     manual, exempt) record for (provider, AdapterVersion()) under the
//     current suite version (BIL-PROV-1).
//
// The registry re-reads provider_certifications every
// PROVIDER_REGISTRY_REFRESH, so an approved record (B-O17) or a revocation
// (B-O15) takes effect without a restart.
package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// Status is one provider's certification state, for logs and the
// commercial_billing_provider_certified gauge.
type Status struct {
	Provider       string
	AdapterVersion string
	Enabled        bool
}

// Registry maps provider names to adapters and tracks which are enabled.
type Registry struct {
	loaded       map[string]port.PaymentProvider
	certs        port.CertificationRepo
	suiteVersion string
	log          port.Logger

	mu      sync.RWMutex
	enabled map[string]bool
}

// New builds the registry from the compiled adapters listed in enabledNames.
// It fails only if a listed name has no compiled adapter (§12).
func New(compiled []port.PaymentProvider, enabledNames []string, certs port.CertificationRepo,
	suiteVersion string, log port.Logger) (*Registry, error) {
	byName := make(map[string]port.PaymentProvider, len(compiled))
	for _, p := range compiled {
		byName[p.Name()] = p
	}
	r := &Registry{loaded: map[string]port.PaymentProvider{}, certs: certs, suiteVersion: suiteVersion, log: log, enabled: map[string]bool{}}
	for _, n := range enabledNames {
		p, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("ENABLED_PAYMENT_PROVIDERS lists %q but no adapter is compiled for it", n)
		}
		r.loaded[n] = p
	}
	return r, nil
}

// Loaded returns the adapter for an existing account's provider. A loaded
// provider serves running work whether or not it is certified (BIL-PROV-2).
func (r *Registry) Loaded(name string) (port.PaymentProvider, bool) {
	p, ok := r.loaded[name]
	return p, ok
}

// ForNewAccount returns the adapter for a new billing account, or
// 503 PROVIDER_NOT_ENABLED when it is not certified (BIL-PROV-1).
func (r *Registry) ForNewAccount(name string) (port.PaymentProvider, error) {
	p, ok := r.loaded[name]
	if !ok {
		return nil, port.ProviderNotEnabled(name, "is not loaded")
	}
	r.mu.RLock()
	enabled := r.enabled[name]
	r.mu.RUnlock()
	if !enabled {
		return nil, port.ProviderNotEnabled(name, "has no valid certification for this adapter version")
	}
	return p, nil
}

// Refresh re-reads the certification of every loaded provider. A provider
// without a valid record stays loaded, is not enabled and is logged at ERROR
// (BillingProviderNotCertified fires from the gauge). On a read failure the
// previous state is kept, so a database blip never disables running work.
func (r *Registry) Refresh(ctx context.Context) ([]Status, error) {
	next := make(map[string]bool, len(r.loaded))
	statuses := make([]Status, 0, len(r.loaded))
	for _, name := range r.names() {
		p := r.loaded[name]
		_, ok, err := r.certs.Enabling(ctx, name, p.AdapterVersion(), r.suiteVersion)
		if err != nil {
			return nil, fmt.Errorf("read certification for %s: %w", name, err)
		}
		next[name] = ok
		statuses = append(statuses, Status{Provider: name, AdapterVersion: p.AdapterVersion(), Enabled: ok})
		if !ok && r.log != nil {
			r.log.Error("payment provider not certified for new accounts (BIL-PROV-1)", map[string]any{
				"provider": name, "adapter_version": p.AdapterVersion(), "suite_version": r.suiteVersion,
			})
		}
	}
	r.mu.Lock()
	r.enabled = next
	r.mu.Unlock()
	return statuses, nil
}

// Run refreshes every interval until ctx ends. onStatus, if set, receives
// each refresh's statuses (for the certified gauge).
func (r *Registry) Run(ctx context.Context, interval time.Duration, onStatus func([]Status)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, err := r.Refresh(ctx)
			if err != nil {
				if r.log != nil {
					r.log.Warn("provider registry refresh failed; keeping previous state", map[string]any{"error": err.Error()})
				}
				continue
			}
			if onStatus != nil {
				onStatus(st)
			}
		}
	}
}

func (r *Registry) names() []string {
	out := make([]string, 0, len(r.loaded))
	for n := range r.loaded {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
