# Provider certification (cmd/certify)

LLD §14.7, §19.6, BIL-PROV-1..4. Run for every new adapter, adapter version
or pinned provider API version. Until a record for the deployed
`adapter_version` is `passed`, the provider takes no new accounts
([certification-lapsed.md](certification-lapsed.md)).

## 1. Run the suite in test mode

```bash
make build                                   # bin/certify
export CERTIFY_SERVER_DSN=postgres://postgres:…@localhost:5432/postgres?sslmode=disable   # scratch server, superuser
export STRIPE_API_KEY=rk_test_…              # the certification test account
bin/certify run --target test-mode --out record.json --evidence-dir evidence \
  --evidence-base-uri s3://commercial-cert-evidence
```

- The run creates database `certify_<run>` on the scratch server, migrates it,
  runs Billing's app layer against it and drops it at the end. Never point
  `CERTIFY_SERVER_DSN` at a shared environment.
- Every Stripe customer is created on a Test Clock, so renewals and dunning
  run in minutes; the clock and its objects are deleted at the end.
- Events are read from the account's event list in raw form, signed with the
  run's own webhook secret and posted to Billing's B-W1 handler in process
  (no public endpoint is needed).
- IAM (Core, Realm Provisioner) and Entitlements are stubbed: the suite
  certifies the provider and Billing's mapping of it.
- Exit 0 only when every required case passed (30: the `cert-1.0` cases of
  LLD §14.7 plus the proration case). `collection_pause` is recorded, not
  required.

## 2. Upload the evidence and submit (B-O14)

```bash
aws s3 sync evidence s3://commercial-cert-evidence
export BILLING_URL=https://api.<env>.bcbpsolutions.com OPERATOR_TOKEN=…   # your operator token
bin/certify submit --record record.json --reason "Stripe adapter <version> certification"
```

`submit` refuses a stripe-mock rehearsal and a record with a failed required
case; the server answers `422 CERTIFICATION_INCOMPLETE` / `SUITE_VERSION_MISMATCH`
/ `409 CERTIFICATION_EXISTS` per §5.4.4. The record is `pending_review`.

## 3. Review (B-O17) — a different PCI owner group member

```bash
OPERATOR_TOKEN=<reviewer's token> bin/certify review --id <id> --decision approve \
  --reason "Evidence reviewed: …"
```

The submitter cannot approve their own run (`422 SELF_REVIEW_NOT_ALLOWED`);
the reviewer needs `pci_owner_reviewer` (BIL-PROV-4). The registry enables the
provider within `PROVIDER_REGISTRY_REFRESH`.

## Release gate

```bash
CERTIFY_CHECK_DSN=<admin_readonly DSN of the environment> \
  bin/certify check stripe "$(bin/certify adapter-version)"
```

Exit 0 only when a `passed` record exists for the provider, adapter version
and suite version (§14.6, BIL-PROV-2).

## Rehearsal against stripe-mock

`go test -tags=integration ./test/certify/` (or `make certify-rehearsal`)
runs the whole suite against stripe-mock and a throwaway Postgres, then the
submit → self-review refused → second reviewer → gate path. stripe-mock is
stateless, so the rehearsal checks request shapes and Billing's mapping;
where Billing needs provider state, the suite substitutes it and lists each
substitution in the case's evidence (`mock_substitutions`). A rehearsal
record carries `provider_account_ref: stripe-mock` and is never submittable.
