package domain

import (
	"errors"
	"testing"
	"time"
)

func utc(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.UTC) }

// TestBIL_SEAT_5_ProrationGoldenTable is the §14.1 golden table for the
// fallback calculator (§8.5.3): monthly and annual, 28/29/30/31-day months,
// a leap year, .5 rounding, and below-minimum in both modes. Every row
// asserts the stored inputs and the unrounded amount as well as the result.
func TestBIL_SEAT_5_ProrationGoldenTable(t *testing.T) {
	cases := []struct {
		name                  string
		in                    ProrationInput
		remaining, period     int64
		unrounded             string
		rounded, charged, def int64
		deferral              string
	}{
		{
			name:      "LLD example: 5 seats, 18 of 30 days",
			in:        ProrationInput{UnitAmountMinor: 6900, SeatsAdded: 5, PeriodStart: utc(2026, 11, 1, 0), PeriodEnd: utc(2026, 12, 1, 0), ProrationInstant: utc(2026, 11, 13, 0), MinChargeMinor: 200, Policy: FreeUntilRenewal},
			remaining: 1555200, period: 2592000, unrounded: "20700.000000000000", rounded: 20700, charged: 20700,
		},
		{
			name:      "LLD example: below minimum, 12 hours of 30 days, free_until_renewal",
			in:        ProrationInput{UnitAmountMinor: 6900, SeatsAdded: 1, PeriodStart: utc(2026, 11, 1, 0), PeriodEnd: utc(2026, 12, 1, 0), ProrationInstant: utc(2026, 11, 30, 12), MinChargeMinor: 200, Policy: FreeUntilRenewal},
			remaining: 43200, period: 2592000, unrounded: "115.000000000000", rounded: 115, charged: 0, def: 115, deferral: DeferralBelowProviderMinimum,
		},
		{
			name:      "below minimum, charge_minimum mode",
			in:        ProrationInput{UnitAmountMinor: 6900, SeatsAdded: 1, PeriodStart: utc(2026, 11, 1, 0), PeriodEnd: utc(2026, 12, 1, 0), ProrationInstant: utc(2026, 11, 30, 12), MinChargeMinor: 200, Policy: ChargeMinimum},
			remaining: 43200, period: 2592000, unrounded: "115.000000000000", rounded: 115, charged: 200,
		},
		{
			name:      "28-day month (Feb 2027), 10 days left",
			in:        ProrationInput{UnitAmountMinor: 6900, SeatsAdded: 1, PeriodStart: utc(2027, 2, 1, 0), PeriodEnd: utc(2027, 3, 1, 0), ProrationInstant: utc(2027, 2, 19, 0), MinChargeMinor: 200, Policy: FreeUntilRenewal},
			remaining: 864000, period: 2419200, unrounded: "2464.285714285714", rounded: 2464, charged: 2464,
		},
		{
			name:      "29-day month (leap Feb 2028), 1 day left, below minimum",
			in:        ProrationInput{UnitAmountMinor: 2900, SeatsAdded: 1, PeriodStart: utc(2028, 2, 1, 0), PeriodEnd: utc(2028, 3, 1, 0), ProrationInstant: utc(2028, 2, 29, 0), MinChargeMinor: 200, Policy: FreeUntilRenewal},
			remaining: 86400, period: 2505600, unrounded: "100.000000000000", rounded: 100, charged: 0, def: 100, deferral: DeferralBelowProviderMinimum,
		},
		{
			name:      ".5 minor unit rounds up (30-day month, 15 days left, odd price)",
			in:        ProrationInput{UnitAmountMinor: 6901, SeatsAdded: 1, PeriodStart: utc(2026, 11, 1, 0), PeriodEnd: utc(2026, 12, 1, 0), ProrationInstant: utc(2026, 11, 16, 0), MinChargeMinor: 200, Policy: FreeUntilRenewal},
			remaining: 1296000, period: 2592000, unrounded: "3450.500000000000", rounded: 3451, charged: 3451,
		},
		{
			name:      "31-day month, 7 days left, 12th place rounds up",
			in:        ProrationInput{UnitAmountMinor: 3500, SeatsAdded: 2, PeriodStart: utc(2026, 12, 1, 0), PeriodEnd: utc(2027, 1, 1, 0), ProrationInstant: utc(2026, 12, 25, 0), MinChargeMinor: 200, Policy: FreeUntilRenewal},
			remaining: 604800, period: 2678400, unrounded: "1580.645161290323", rounded: 1581, charged: 1581,
		},
		{
			name:      "annual, 365-day year, 73 days left",
			in:        ProrationInput{UnitAmountMinor: 70380, SeatsAdded: 1, PeriodStart: utc(2027, 1, 1, 0), PeriodEnd: utc(2028, 1, 1, 0), ProrationInstant: utc(2027, 10, 20, 0), MinChargeMinor: 200, Policy: FreeUntilRenewal},
			remaining: 6307200, period: 31536000, unrounded: "14076.000000000000", rounded: 14076, charged: 14076,
		},
		{
			name:      "annual, leap year 366 days, half left",
			in:        ProrationInput{UnitAmountMinor: 182580, SeatsAdded: 1, PeriodStart: utc(2028, 1, 1, 0), PeriodEnd: utc(2029, 1, 1, 0), ProrationInstant: utc(2028, 7, 2, 0), MinChargeMinor: 200, Policy: FreeUntilRenewal},
			remaining: 15811200, period: 31622400, unrounded: "91290.000000000000", rounded: 91290, charged: 91290,
		},
		{
			name:      "exactly the minimum is charged, not deferred",
			in:        ProrationInput{UnitAmountMinor: 6000, SeatsAdded: 1, PeriodStart: utc(2026, 11, 1, 0), PeriodEnd: utc(2026, 12, 1, 0), ProrationInstant: utc(2026, 11, 30, 0), MinChargeMinor: 200, Policy: FreeUntilRenewal},
			remaining: 86400, period: 2592000, unrounded: "200.000000000000", rounded: 200, charged: 200,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Prorate(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got.UnitAmountMinor != tc.in.UnitAmountMinor || got.RemainingSeconds != tc.remaining || got.PeriodSeconds != tc.period {
				t.Errorf("inputs = %d/%d/%d, want %d/%d/%d", got.UnitAmountMinor, got.RemainingSeconds, got.PeriodSeconds, tc.in.UnitAmountMinor, tc.remaining, tc.period)
			}
			if got.AmountUnrounded != tc.unrounded {
				t.Errorf("unrounded = %s, want %s", got.AmountUnrounded, tc.unrounded)
			}
			if got.RoundedMinor != tc.rounded || got.AmountMinor != tc.charged {
				t.Errorf("rounded/charged = %d/%d, want %d/%d", got.RoundedMinor, got.AmountMinor, tc.rounded, tc.charged)
			}
			if got.DeferredAmountMinor != tc.def || got.DeferralReason != tc.deferral {
				t.Errorf("deferral = %d %q, want %d %q", got.DeferredAmountMinor, got.DeferralReason, tc.def, tc.deferral)
			}
			if got.Deferred() != (tc.deferral != "") {
				t.Error("Deferred() disagrees with DeferralReason")
			}
		})
	}
}

func TestProrationPreviewEqualsCharge(t *testing.T) {
	in := ProrationInput{UnitAmountMinor: 6900, SeatsAdded: 3, PeriodStart: utc(2026, 11, 1, 0), PeriodEnd: utc(2026, 12, 1, 0), ProrationInstant: time.Date(2026, 11, 7, 13, 21, 9, 500, time.UTC), MinChargeMinor: 200, Policy: FreeUntilRenewal}
	a, _ := Prorate(in)
	b, _ := Prorate(in)
	if a != b {
		t.Fatal("same proration_instant must give identical preview and charge (§8.5.3)")
	}
}

func TestProrationRejectsBadInput(t *testing.T) {
	base := ProrationInput{UnitAmountMinor: 6900, SeatsAdded: 1, PeriodStart: utc(2026, 11, 1, 0), PeriodEnd: utc(2026, 12, 1, 0), ProrationInstant: utc(2026, 11, 2, 0), MinChargeMinor: 200, Policy: FreeUntilRenewal}
	mod := func(f func(*ProrationInput)) ProrationInput { c := base; f(&c); return c }
	cases := map[string]struct {
		in   ProrationInput
		want error
	}{
		"zero seats":         {mod(func(i *ProrationInput) { i.SeatsAdded = 0 }), ErrProrationSeats},
		"negative price":     {mod(func(i *ProrationInput) { i.UnitAmountMinor = -1 }), ErrProrationPrice},
		"empty period":       {mod(func(i *ProrationInput) { i.PeriodEnd = i.PeriodStart }), ErrProrationPeriod},
		"instant before":     {mod(func(i *ProrationInput) { i.ProrationInstant = utc(2026, 10, 31, 0) }), ErrProrationInstant},
		"instant at the end": {mod(func(i *ProrationInput) { i.ProrationInstant = i.PeriodEnd }), ErrProrationInstant},
		"unknown policy":     {mod(func(i *ProrationInput) { i.Policy = "round" }), ErrProrationPolicy},
	}
	for name, tc := range cases {
		if _, err := Prorate(tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}
