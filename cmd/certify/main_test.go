package main

import (
	"bytes"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/certify"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/domain"
)

func fullRecord(account string) certify.Record {
	rec := certify.Record{Provider: "stripe", SuiteVersion: certify.SuiteVersion, ProrationMode: "native", ProviderAccountRef: account}
	for group, cases := range domain.CertSuite[certify.SuiteVersion] {
		for _, c := range cases {
			rec.Results = append(rec.Results, app.CertificationResult{Capability: group, Case: c, Passed: true})
		}
	}
	rec.Results = append(rec.Results, app.CertificationResult{Capability: "proration", Case: "preview_equals_charge", Passed: true})
	return rec
}

// A stripe-mock rehearsal or an incomplete run is never submitted.
func TestSubmittable(t *testing.T) {
	if err := submittable(fullRecord("acct_test_1")); err != nil {
		t.Fatalf("complete test-mode record refused: %v", err)
	}
	if submittable(fullRecord(certify.MockAccountRef)) == nil {
		t.Error("stripe-mock record accepted")
	}
	rec := fullRecord("acct_test_1")
	rec.Results[0].Passed = false
	if submittable(rec) == nil {
		t.Error("record with a failed required case accepted")
	}
	rec = fullRecord("acct_test_1")
	rec.Results = rec.Results[:len(rec.Results)-1] // no proration case
	if submittable(rec) == nil {
		t.Error("record without the proration case accepted")
	}
}

func TestUsage(t *testing.T) {
	var out, errb bytes.Buffer
	for _, args := range [][]string{nil, {"nope"}, {"run", "--target", "prod"}, {"submit"}, {"review", "--id", "x"}, {"check", "stripe"}} {
		if got := run(args, &out, &errb); got != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, got, exitUsage)
		}
	}
}
