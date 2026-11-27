package metrics

import (
	"net/http"
	"testing"
)

// Lines without a label set or value, and a label with no closing quote,
// are tolerated by the filter.
func TestExpositionHelpers(t *testing.T) {
	if got := sampleFamily("commercial_billing_x"); got != "commercial_billing_x" {
		t.Errorf("sampleFamily = %q", got)
	}
	if _, ok := labelValue(`x{job_name="seat-renewal} 1`, "job_name"); ok {
		t.Error("unterminated label value read")
	}
	if v, ok := labelValue(`x{job_name="seat-renewal"} 1`, "job_name"); !ok || v != "seat-renewal" {
		t.Errorf("labelValue = %q %v", v, ok)
	}
}

func TestBufferWriterRecordsStatus(t *testing.T) {
	w := &bufferWriter{header: http.Header{}, code: http.StatusOK}
	w.WriteHeader(http.StatusServiceUnavailable)
	if _, err := w.Write([]byte("x")); err != nil || w.code != http.StatusServiceUnavailable || w.body.String() != "x" {
		t.Errorf("code %d body %q", w.code, w.body.String())
	}
}
