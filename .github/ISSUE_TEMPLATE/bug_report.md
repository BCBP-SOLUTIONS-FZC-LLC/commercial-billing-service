---
name: Bug report
about: A defect in commercial-billing (HTTP API, webhook, events, jobs, provider adapter, database)
title: '[BUG] '
labels: bug
assignees: ''
---

> **Security issue** (card data, double charge, forged webhook, cross-tenant access)? Do not file it here — see `.github/SECURITY.md`.

## Description

## Version
Commit SHA / tag:
LLD revision:

## Environment
- [ ] Local (`make run`)
- [ ] Tests (`make test` / `make test-integration`)
- [ ] Staging
- [ ] Production

## Affected area
- [ ] Tenant route (ID: `B-P…`)
- [ ] B-I1 direct signup
- [ ] B-W1 webhook / ledger processor
- [ ] Operator route (ID: `B-O…`)
- [ ] Produced event (type: `Tenant…`)
- [ ] Consumed event (`tenant-billing-q` / `usage-billing-q`)
- [ ] CronJob (`--job=…`)
- [ ] Provider adapter (Stripe / manual) or certification
- [ ] Database / migration / RLS
- [ ] Helm / deployment

## Money or tenant impact
- [ ] A charge, refund, seat count or price could be wrong
- [ ] An event could be lost, duplicated or out of order
- [ ] Data from one tenant visible to another
- [ ] None

## Steps to reproduce
1.
2.

## Expected behaviour

## Actual behaviour
Status codes, `errorCode`, log lines (no card data or secrets).

```
```

## Invariant (if known)
`BIL-…`
