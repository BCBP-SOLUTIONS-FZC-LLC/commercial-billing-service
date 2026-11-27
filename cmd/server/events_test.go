package main

import (
	"testing"
	"time"
)

// platform-events refuses a lease below BatchSize×PublishTimeout + 1 min.
func TestOutboxClaimLease(t *testing.T) {
	for _, batch := range []int{1, 50, 500} {
		if got, min := outboxClaimLease(batch), time.Duration(batch)*outboxPublishTimeout+time.Minute; got < min || got < 10*time.Minute {
			t.Errorf("batch %d: lease %s below %s", batch, got, min)
		}
	}
}
