---
name: Feature request
about: A new route, event, field, flow or operational behaviour for commercial-billing
title: '[FEAT] '
labels: enhancement
assignees: ''
---

> Billing implements its LLD. A change in behaviour starts as an LLD change (with a revision row) agreed with the Commercial team. Link the LLD section, or say that one is needed.

## Problem / motivation
Who needs it (tenant admin, Signup BFF, Core, Entitlements, operators, Finance)? What decision or record does it change?

## Proposal

## LLD
Section(s): §… · New revision needed: yes / no

## Affected areas
- [ ] Tenant / internal / operator route
- [ ] Produced or consumed event (asyncapi, consumers to coordinate)
- [ ] Database (migration)
- [ ] Provider adapter (new certification run)
- [ ] Configuration / feature flag
- [ ] Metrics, alerts, runbooks
- [ ] Helm / network policy

## Money, PCI and invariants
Does it move money, change a price, touch card-adjacent data or an invariant (`BIL-…`)?

## Alternatives considered

## Acceptance criteria
- [ ]
