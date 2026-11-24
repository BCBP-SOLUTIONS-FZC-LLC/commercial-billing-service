# Availability error budget burning

**Alerts:** `BillingAvailabilityBudgetBurnFast` (SEV-2: 14.4× the budget rate over 1 h and 5 m), `BillingAvailabilityBudgetBurnSlow` (SEV-3: 6× over 6 h and 30 m). SLO: 99.9 % of tenant, internal and operator requests without a 5xx over 30 days (LLD §11.1; `deploy/monitoring/slo-rules.yml`).

1. Find the failing routes: `sum by (route) (rate(platform_http_requests_total{service="commercial-billing",status_class="5xx"}[5m]))`.
2. Read the problems' `errorCode` in the logs (`problem response` entries) for those routes:
   - `PROVIDER_UNAVAILABLE` → [provider-outage.md](provider-outage.md);
   - `IAM_UNAVAILABLE` → Core / Realm Provisioner health ([i18-unavailable.md](i18-unavailable.md));
   - `DB_UNAVAILABLE`, `REQUEST_TIMEOUT` → database (pool saturation `platform_db_*`, locks, failover);
   - `INTERNAL` → a defect: roll back the last deploy if it started with it.
3. Dependency error ratios: `commercial_billing:dependency_errors:ratio_rate5m`.
4. The fast burn exhausts a month's budget in about two days; page until the ratio is back under 0.1 %.
