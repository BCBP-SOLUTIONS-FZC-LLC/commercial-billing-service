# Schema registration

**Alerts** (`deploy/monitoring/schema-registry-alerts.yml`, all SEV-3):
- `BillingSchemaRegistrationFailed`;
- `BillingSchemaBreakingChangeBlocked`;
- `BillingSchemaRegistrationSpike`;
- `BillingSchemaVersionChurn`.

They come from `schema-gov metrics`, pushed to the Pushgateway at the end of each `schema-registry.yml` run. They fire only when the environment has the `PUSHGATEWAY_URL` secret.

**Why it matters:**
- **Deploys depend on it.** At startup the server resolves each produced schema in Glue `billing-events` by definition (LLD §7.6), and refuses to start if one is missing. A schema change that isn't registered in an environment therefore blocks every deploy there.
- **Releases depend on it.** `release.yml`'s `schema-verify` job fails for the same reason.

1. **`BillingSchemaRegistrationFailed`.** Open the last `Glue Schema Registry` workflow run for the environment.
   - **Freeze check failed:** `SCHEMA_FREEZE` is set. Lift it, or re-run with `force=true` once the freeze owner agrees.
   - **AWS error:** check the CI principal's Glue permissions (`deploy/iam/policy.tf.example`, CI role) and `AWS_REGION`.
   - **Diff failed:** see step 2.
   - **`make schema-verify` failed after registering:** a version is not `AVAILABLE` yet. Re-run the workflow.

   Re-run with `workflow_dispatch`. The alert clears when a run succeeds.
2. **`BillingSchemaBreakingChangeBlocked`.** A PR changed a produced schema incompatibly; the log names the event type.
   - Do not force it through.
   - Publish a new event type (`<Event>V2`) and dual-publish it for one release.
   - Coordinate the consumers listed in `api/asyncapi.yaml` (Entitlements reads `TenantQuotaAddOnChanged`; see `test/contract`).
   - Follow CONTRIBUTING.md, "Adding a domain event".
3. **`BillingSchemaRegistrationSpike`.** One run processed more than Billing's six produced schemas.
   - Check `make _stage-produced` (`.tmp/glue/` must hold exactly the six `tenant_*.json` schemas, under their event-type names).
   - Check `GLUE_REGISTRY_NAME` in the workflow.
   - Remove anything registered by mistake only after confirming that no producer uses it.
4. **`BillingSchemaVersionChurn`.** One schema gained more than three versions in seven days. Each version is migration work for consumers, so batch the changes or stabilise the contract.
