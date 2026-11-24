# Core I-18 unavailable

**Alert:** `BillingI18Unavailable` (SEV-3): every plan-change check against Core (`GET /api/v1/internal/tenants/:id/plan-change-check`) has failed for 10 minutes. LLD §8.4.2, §20.7.

**Effect:** downgrades fail closed: B-P4 answers `503 IAM_UNAVAILABLE`. Scheduled downgrades re-checked by `downgrade-apply` keep retrying; one still unchecked 15 minutes before its renewal is released and marked `blocked` (`iam_unavailable`), so the subscription renews on the current plan. Upgrades are never blocked.

1. Check Core (`iam-org-membership`) health and its RP-11 dependency; I-18 calls RP inside.
2. Check the NetworkPolicy / AuthorizationPolicy admitting `commercial-billing` to Core (CD-19) if errors are 403.
3. After recovery, tenants whose downgrade was blocked by `iam_unavailable` can request it again (B-P4).
