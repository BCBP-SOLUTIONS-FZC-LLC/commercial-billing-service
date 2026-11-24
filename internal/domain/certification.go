package domain

// CertSuite is the required cases of one certification suite version
// (LLD §14.7), as capability group → case ids. cmd/certify writes results
// with these ids; a run missing any is CERTIFICATION_INCOMPLETE. Proration
// is checked separately: native needs "preview_equals_charge", fallback
// needs "calculator_charged" (§14.7, BIL-PROV-3).
var CertSuite = map[string]map[string][]string{
	"cert-1.0": {
		"recurring_subscriptions": {"create", "increase_quantity", "change_plan_now", "change_plan_period_end",
			"schedule_lower_quantity", "renew", "cancel_at_period_end", "cancel_now"},
		"webhook_delivery":      {"signature_verified", "tolerance_enforced", "redelivered", "out_of_order_converges"},
		"stable_event_ids":      {"same_id_on_redelivery", "ledger_dedupe"},
		"invoice_retrieval":     {"by_reference", "pdf_link", "listing_paginates"},
		"payment_method_update": {"hosted_page", "used_for_renewal", "no_card_data"},
		"refunds":               {"full", "partial", "event_recorded"},
		"retry_semantics":       {"retried", "exhausted_past_due_cancelled", "later_success_active"},
		"one_off_charges":       {"off_session", "three_d_secure", "decline"},
	},
}

// CertResult is one case result of a run.
type CertResult struct {
	Capability string
	Case       string
	Passed     bool
}

// CertOutcome evaluates a run: the required cases not passed, and whether
// the declared proration mode is proven. status is pending_review when
// nothing is missing, else failed (§5.4.4 B-O14).
func CertOutcome(suite, prorationMode, declaredProration string, results []CertResult) (missing []string, prorationOK bool, known bool) {
	req, known := CertSuite[suite]
	if !known {
		return nil, false, false
	}
	passed := map[string]bool{}
	for _, r := range results {
		if r.Passed {
			passed[r.Capability+"/"+r.Case] = true
		}
	}
	for group, cases := range req {
		for _, c := range cases {
			if !passed[group+"/"+c] {
				missing = append(missing, group+"/"+c)
			}
		}
	}
	switch prorationMode {
	case "native":
		prorationOK = declaredProration == "native" && passed["proration/preview_equals_charge"]
	case "fallback":
		prorationOK = declaredProration == "fallback" && passed["proration/calculator_charged"]
	}
	return missing, prorationOK, true
}
