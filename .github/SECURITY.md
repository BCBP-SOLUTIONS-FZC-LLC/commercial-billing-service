# Security Policy

`commercial-billing` handles payment flows. Its PCI scope is SAQ A: card data never reaches it.
The browser tokenises cards with the provider's JavaScript, and Billing stores provider ids only (LLD §10.1).

## Supported versions

| Version | Supported |
|---------|-----------|
| *(none released)* | The service has no tagged release and is not deployed. Report issues against `main` or the current phase branch. |

Once `v1.0.0` ships, the latest `1.x` image is supported (VERSIONING.md).

## Reporting a vulnerability

**Do not open a public GitHub issue for a security vulnerability.**

Email: vijay@bcbpsolutions.com
Subject: `[commercial-billing] Security vulnerability`

Include:
- the vulnerability and the affected component (route ID, webhook, consumer, CronJob, provider adapter, Helm chart);
- steps to reproduce;
- the impact (card data exposure, double charge, forged payment confirmation, cross-tenant access, privilege escalation, DoS, …);
- a suggested fix, if any.

### Response timeline

| Step | Target |
|------|--------|
| Acknowledgement | 48 hours |
| Severity assessment | 5 business days |
| Fix for critical / high | 14 days |
| Public disclosure | After the fix ships |

We follow responsible disclosure and credit reporters in the release notes unless asked not to. A finding that touches PCI scope is also reported to the PCI owner group.

## Scope — the most sensitive areas

- **Card data and secrets** (`BIL-PCI-1..3`, `BIL-API-4`). No PAN, CVV, expiry, provider key or webhook secret may appear in a table, log, event, span, error or response. `client_secret` is returned only in the 3-D Secure action to the payer.
- **Provider callbacks (B-W1)** (`BIL-PAY-1..5`):
  - forged or replayed webhooks;
  - signature, tolerance or `livemode` bypass;
  - trusting payload content over the re-read;
  - resolving a tenant from metadata.
- **Charging routes** (`BIL-IDEM-*`, `BIL-CONC-*`): double charges through retries, concurrency or idempotency-key reuse.
- **Tenant isolation** (`BIL-API-1..2`, `BIL-RLS-1..3`):
  - identity taken from a body;
  - a path tenant that differs from `x-tenant-id`;
  - an RLS bypass;
  - a session-scoped `app.tenant_id` leaking across pooled connections.
- **Caller identity:**
  - B-I1 accepting a caller other than the Signup BFF's mesh principal (`BIL-AUTH-3`);
  - `commercial-system` accepted inbound (`BIL-AUTH-4`);
  - operator routes reachable before the operator path is live (`BIL-AUTH-6`).
- **Provider certification** (`BIL-PROV-1..4`): a provider taking new accounts without a passed certification, or one person approving their own run.
- **Supply chain:**
  - base images (digest-pinned);
  - dependencies (govulncheck, Trivy);
  - the signed image (Cosign, SLSA provenance).
