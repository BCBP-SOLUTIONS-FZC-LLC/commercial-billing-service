# Stripe Test Clock rehearsal (go-live evidence)

LLD §14.4; rev 0.36 makes these runs go-live evidence. Staging only, Stripe
**test mode** (the script refuses a live key, BIL-OPS-7).

## How it works

Stripe can only advance a customer that was created on a Test Clock, so each
scenario:
1. creates a trial tenant in IAM staging (`NEW_TENANT_CMD`) and converts it
   with a manual stub (B-O1);
2. creates a Stripe customer on a fresh Test Clock and a subscription with
   Billing's metadata and `trial_end` = the stub's period end;
3. links them with the stub → Stripe switch of LLD §19.3 (one transaction on
   `billing_accounts`, `subscriptions`, `subscription_items`; staging DB);
4. advances the clock and asserts Billing's state through the database and
   the API (webhooks, processor, CronJobs and events run as in production).

## Run

```bash
export STRIPE_TEST_KEY=rk_test_…            # staging restricted test key
export BILLING_URL=https://api.staging.bcbpsolutions.com
export TOKEN_CMD=…                          # prints a bearer token: <role> <tenant_id>
export NEW_TENANT_CMD=…                     # prints a new trial tenant id
export PSQL_URL=postgres://…                # operator role on the staging DB
export SUSPEND_CMD=… REACTIVATE_CMD=…       # IAM operator suspend / reactivate (scenario 4)
scripts/testclock/run.sh                    # all scenarios, or name them
```

| Scenario | LLD §14.4 case | Automated |
|---|---|---|
| `renewal_month`, `renewal_year` | monthly and annual renewal | yes |
| `dunning` | failed renewal → `past_due` → retries → `cancelled` | yes |
| `suspension` | operator suspension across a renewal (draft held, no charge) → reactivation (held draft not finalised) → B-O16 | yes, with the IAM hooks |
| `reactivation_past_due` | reactivation from `past_due` | yes |
| `downgrade` | scheduled downgrade with the I-18 re-check | yes (downgrade-apply runs on its schedule) |
| `reduction` | reduction across a renewal | yes (trigger `seat-renewal` or wait 15 min) |
| `addon_3ds` | token add-on with 3-D Secure test cards | manual (browser challenge; `FEATURE_TOKEN_ADDONS` on in staging) |

Reactivation from `cancelled` and the open-`past_due`-during-suspension
branch are run by hand from the same building blocks (`lib.sh`) until the
IAM staging hooks cover them.

## Evidence

Each scenario appends one JSON line to `evidence/testclock-<run_id>.jsonl`:
the Test Clock id, tenant, Billing subscription, provider subscription and
final state. Attach the file and the run log to the go-live ticket.

## Cleanup

Test Clocks delete their customers and subscriptions when deleted
(`DELETE /v1/test_helpers/test_clocks/<id>`). The staging tenants stay as
financial-record-like test data (CD-14 does not apply to staging; remove
them with the staging data reset).
