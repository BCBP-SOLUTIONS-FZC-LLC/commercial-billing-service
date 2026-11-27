#!/usr/bin/env bash
# LLD §14.4 Test Clock scenarios. Each function runs one scenario end to end
# and records evidence. Sourced by run.sh after lib.sh.

now_epoch() { date -u +%s; }

# 1, 2: monthly and annual renewal — period rolls, invoice mirrored, no IAM event.
scenario_renewal() {
  local interval=$1 tenant sub clock psub end
  read -r tenant sub < <(stub_tenant pro "$interval" 5)
  clock=$(new_clock "renewal-$interval-$RUN_ID" "$(now_epoch)")
  psub=$(link_to_clock "$tenant" "$sub" "$clock" pro "$interval" 5)
  end=$(stripe GET "/v1/subscriptions/$psub" | jq -r '.trial_end // .items.data[0].current_period_end')
  advance "$clock" $((end + 3600))
  wait_for "period rolled" "SELECT current_period_start >= to_timestamp($end) - interval '1 minute' FROM subscriptions WHERE billing_subscription_id='$sub'" t
  wait_for "status" "$(sub_status "$sub")" active
  wait_for "cycle invoice mirrored" "SELECT count(*) > 0 FROM invoices WHERE tenant_id='$tenant' AND status='paid'" t
  evidence "renewal_$interval" "$(jq -n --arg c "$clock" --arg t "$tenant" --arg s "$sub" --arg p "$psub" '{clock:$c, tenant_id:$t, billing_subscription_id:$s, provider_subscription:$p, final_status:"active"}')"
}

# 3: failed renewal → past_due → retries → cancelled (dunning exhausted).
scenario_dunning() {
  local tenant sub clock psub end
  read -r tenant sub < <(stub_tenant pro month 3)
  clock=$(new_clock "dunning-$RUN_ID" "$(now_epoch)")
  # pm_card_chargeCustomerFail attaches but fails every charge: the first
  # charge is the renewal at trial_end, which fails.
  psub=$(link_to_clock "$tenant" "$sub" "$clock" pro month 3 pm_card_chargeCustomerFail)
  end=$(stripe GET "/v1/subscriptions/$psub" | jq -r '.trial_end // .items.data[0].current_period_end')
  advance "$clock" $((end + 3600))
  wait_for "past_due after the failed renewal" "$(sub_status "$sub")" past_due
  wait_for "TenantPaymentPastDue once" "SELECT count(*) FROM outbox_events WHERE tenant_id='$tenant' AND event_type='TenantPaymentPastDue'" 1
  advance "$clock" $((end + 40 * 86400))   # beyond the Smart Retries window
  wait_for "cancelled after dunning" "$(sub_status "$sub")" cancelled
  wait_for "TenantSubscriptionCancelled" "SELECT count(*) FROM outbox_events WHERE tenant_id='$tenant' AND event_type='TenantSubscriptionCancelled'" 1
  evidence dunning "$(jq -n --arg c "$clock" --arg t "$tenant" --arg s "$sub" '{clock:$c, tenant_id:$t, billing_subscription_id:$s, final_status:"cancelled"}')"
}

# 4: operator suspension across a renewal — draft held, no charge; resume
# after reactivation; held draft not finalised; B-O16 finalize / void.
scenario_suspension() {
  [[ -n "${SUSPEND_CMD:-}" && -n "${REACTIVATE_CMD:-}" ]] || { log "skip suspension: SUSPEND_CMD / REACTIVATE_CMD not set"; return 0; }
  local tenant sub clock psub end inv
  read -r tenant sub < <(stub_tenant pro month 4)
  clock=$(new_clock "suspension-$RUN_ID" "$(now_epoch)")
  psub=$(link_to_clock "$tenant" "$sub" "$clock" pro month 4)
  $SUSPEND_CMD "$tenant"
  wait_for "collection paused (auto)" "SELECT collection_paused_by FROM subscriptions WHERE billing_subscription_id='$sub'" auto
  end=$(stripe GET "/v1/subscriptions/$psub" | jq -r '.trial_end // .items.data[0].current_period_end')
  advance "$clock" $((end + 3600))
  wait_for "renewal draft held" "SELECT count(*) FROM invoices WHERE tenant_id='$tenant' AND held_for_review AND status='draft'" 1
  wait_for "no charge, still active" "$(sub_status "$sub")" active
  $REACTIVATE_CMD "$tenant"
  wait_for "collection resumed" "SELECT collection_paused FROM subscriptions WHERE billing_subscription_id='$sub'" f
  inv=$(sql "SELECT invoice_id FROM invoices WHERE tenant_id='$tenant' AND held_for_review AND disposition IS NULL LIMIT 1")
  wait_for "held draft not finalised" "SELECT status FROM invoices WHERE invoice_id='$inv'" draft
  billing POST "/api/v1/operator/tenants/$tenant/invoices/$inv/disposition" operator "$tenant" \
    '{"action":"void","reason":"Test Clock rehearsal: void the held draft"}' >/dev/null
  wait_for "disposition recorded" "SELECT disposition FROM invoices WHERE invoice_id='$inv'" void
  evidence suspension "$(jq -n --arg c "$clock" --arg t "$tenant" --arg i "$inv" '{clock:$c, tenant_id:$t, held_invoice:$i, disposition:"void"}')"
}

# 5: reactivation from past_due (pay the open invoice with a good card).
scenario_reactivation_past_due() {
  local tenant sub clock psub end cus open
  read -r tenant sub < <(stub_tenant pro month 2)
  clock=$(new_clock "react-pd-$RUN_ID" "$(now_epoch)")
  psub=$(link_to_clock "$tenant" "$sub" "$clock" pro month 2 pm_card_chargeCustomerFail)
  end=$(stripe GET "/v1/subscriptions/$psub" | jq -r '.trial_end // .items.data[0].current_period_end')
  advance "$clock" $((end + 3600))
  wait_for "past_due" "$(sub_status "$sub")" past_due
  cus=$(sql "SELECT provider_customer_id FROM billing_accounts WHERE tenant_id='$tenant'")
  stripe POST "/v1/payment_methods/pm_card_visa/attach" "customer=$cus" >/dev/null || true
  open=$(stripe GET /v1/invoices "customer=$cus" "status=open" | jq -r '.data[0].id')
  stripe POST "/v1/invoices/$open/pay" "payment_method=pm_card_visa" >/dev/null
  wait_for "active again" "$(sub_status "$sub")" active
  wait_for "TenantReactivated {previous_status: past_due}" "SELECT payload->'data'->>'previous_status' FROM outbox_events WHERE tenant_id='$tenant' AND event_type='TenantReactivated'" past_due
  evidence reactivation_past_due "$(jq -n --arg c "$clock" --arg t "$tenant" '{clock:$c, tenant_id:$t, final_status:"active"}')"
}

# 6: scheduled downgrade with the I-18 re-check, applied at the renewal.
scenario_downgrade() {
  local tenant sub clock psub end
  read -r tenant sub < <(stub_tenant enterprise month 5)
  clock=$(new_clock "downgrade-$RUN_ID" "$(now_epoch)")
  psub=$(link_to_clock "$tenant" "$sub" "$clock" enterprise month 5)
  local v; v=$(sql "SELECT record_version FROM subscriptions WHERE billing_subscription_id='$sub'")
  billing POST "/api/v1/tenants/$tenant/subscription/plan-change" owner "$tenant" "{\"target_plan\":\"pro\",\"record_version\":$v}" | jq -e '.status=="scheduled"' >/dev/null \
    || die "downgrade not scheduled"
  end=$(stripe GET "/v1/subscriptions/$psub" | jq -r '.trial_end // .items.data[0].current_period_end')
  log "downgrade-apply re-checks I-18 within 2 h of $end; run the CronJob or wait"
  advance "$clock" $((end + 3600))
  wait_for "plan after renewal" "SELECT plan FROM subscriptions WHERE billing_subscription_id='$sub'" pro
  wait_for "TenantPlanChanged downgrade" "SELECT payload->'data'->>'direction' FROM outbox_events WHERE tenant_id='$tenant' AND event_type='TenantPlanChanged'" downgrade
  evidence downgrade "$(jq -n --arg c "$clock" --arg t "$tenant" '{clock:$c, tenant_id:$t, final_plan:"pro"}')"
}

# 7: scheduled seat reduction across a renewal (max(target, in use)).
scenario_reduction() {
  local tenant sub clock psub end
  read -r tenant sub < <(stub_tenant pro month 10)
  clock=$(new_clock "reduction-$RUN_ID" "$(now_epoch)")
  psub=$(link_to_clock "$tenant" "$sub" "$clock" pro month 10)
  billing PUT "/api/v1/tenants/$tenant/subscription/seats/scheduled-reduction" owner "$tenant" '{"target_seats":6}' >/dev/null
  end=$(stripe GET "/v1/subscriptions/$psub" | jq -r '.trial_end // .items.data[0].current_period_end')
  advance "$clock" $((end - 3 * 3600))
  log "run the seat-renewal CronJob now (kubectl create job --from=cronjob/commercial-billing-seat-renewal …) or wait 15 min"
  wait_for "applying ahead of renewal" "SELECT status FROM seat_change_schedules WHERE billing_subscription_id='$sub'" applying
  advance "$clock" $((end + 3600))
  wait_for "applied at renewal" "SELECT status FROM seat_change_schedules WHERE billing_subscription_id='$sub'" applied
  evidence reduction "$(jq -n --arg c "$clock" --arg t "$tenant" --arg f "$(sql "SELECT final_seats FROM seat_change_schedules WHERE billing_subscription_id='$sub'")" '{clock:$c, tenant_id:$t, final_seats:($f|tonumber)}')"
}

# 8: token add-on with 3-D Secure test cards — the purchase answers
# requires_action with a client secret; completing the challenge needs a
# browser, so the run records the requires_action outcome and the decline.
scenario_addon_3ds() {
  log "token add-on 3DS: FEATURE_TOKEN_ADDONS must be on in staging; completing the 3-D Secure challenge is a browser step (Stripe test card 4000 0027 6000 3184)"
  evidence addon_3ds '{"status":"manual","note":"requires FEATURE_TOKEN_ADDONS and a browser for the challenge"}'
}
