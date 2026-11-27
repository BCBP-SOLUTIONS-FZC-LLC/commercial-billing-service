package domain

import (
	"errors"
	"math/big"
	"time"
)

// Billing's proration calculator for providers certified with
// Proration = fallback (LLD §8.5.3, BIL-PROV-3, BIL-SEAT-5). Finance approved
// the rule (BL-OQ-6): exact rational arithmetic on the whole line, rounded
// half-up once, at the end, to the minor unit. Providers with their own
// proration engine (Stripe) stay the system of record and never use this.

// BelowMinimumPolicy is PRORATION_BELOW_MINIMUM (§12).
type BelowMinimumPolicy string

// Policies.
const (
	// FreeUntilRenewal adds the seats without a charge and bills them from
	// the next renewal (Finance approved, the default).
	FreeUntilRenewal BelowMinimumPolicy = "free_until_renewal"
	// ChargeMinimum charges the provider minimum instead. It remains a
	// configuration option but is not used (§8.5.3).
	ChargeMinimum BelowMinimumPolicy = "charge_minimum"
)

// DeferralBelowProviderMinimum is the only deferral_reason value (chk_sp_deferral).
const DeferralBelowProviderMinimum = "below_provider_minimum"

// ProrationInput is one prorated seat line.
type ProrationInput struct {
	UnitAmountMinor  int64     // price per seat per interval, minor units
	SeatsAdded       int64     // > 0
	PeriodStart      time.Time // current period, UTC
	PeriodEnd        time.Time
	ProrationInstant time.Time // the preview's instant; the charge reuses it
	MinChargeMinor   int64     // MIN_CHARGE_MINOR
	Policy           BelowMinimumPolicy
}

// ProrationResult holds everything stored on seat_purchases (BIL-SEAT-5).
type ProrationResult struct {
	UnitAmountMinor  int64
	RemainingSeconds int64
	PeriodSeconds    int64
	// AmountUnrounded is the exact amount in minor units as a decimal with
	// 12 places (amount_unrounded_minor numeric(38,12)).
	AmountUnrounded string
	// RoundedMinor is round_half_up(AmountUnrounded): the prorated amount
	// before any below-minimum policy is applied.
	RoundedMinor int64
	// AmountMinor is what is charged now (0 when deferred).
	AmountMinor int64
	// DeferredAmountMinor and DeferralReason are set only when a
	// free_until_renewal deferral applies.
	DeferredAmountMinor int64
	DeferralReason      string
}

// Deferred reports whether the amount was deferred to the next renewal.
func (r ProrationResult) Deferred() bool { return r.DeferralReason != "" }

// Proration errors.
var (
	ErrProrationSeats   = errors.New("proration: seats_added must be positive")
	ErrProrationPrice   = errors.New("proration: unit_amount_minor must not be negative")
	ErrProrationPeriod  = errors.New("proration: period_end must be after period_start")
	ErrProrationInstant = errors.New("proration: proration_instant must be within [period_start, period_end)")
	ErrProrationPolicy  = errors.New("proration: unknown below-minimum policy")
)

// Prorate computes the fallback proration of §8.5.3:
//
//	remaining_seconds = period_end − proration_instant   (whole seconds, UTC)
//	period_seconds    = period_end − period_start
//	amount_unrounded  = unit × seats × remaining_seconds / period_seconds (exact)
//	amount_minor      = round_half_up(amount_unrounded)
//
// Below MIN_CHARGE_MINOR with free_until_renewal the purchase is confirmed
// with amount_minor = 0 and the rounded amount deferred.
func Prorate(in ProrationInput) (ProrationResult, error) {
	if in.SeatsAdded <= 0 {
		return ProrationResult{}, ErrProrationSeats
	}
	if in.UnitAmountMinor < 0 {
		return ProrationResult{}, ErrProrationPrice
	}
	start, end, at := in.PeriodStart.UTC().Truncate(time.Second), in.PeriodEnd.UTC().Truncate(time.Second), in.ProrationInstant.UTC().Truncate(time.Second)
	if !end.After(start) {
		return ProrationResult{}, ErrProrationPeriod
	}
	if at.Before(start) || !at.Before(end) {
		return ProrationResult{}, ErrProrationInstant
	}
	if in.Policy != FreeUntilRenewal && in.Policy != ChargeMinimum {
		return ProrationResult{}, ErrProrationPolicy
	}

	remaining := int64(end.Sub(at) / time.Second)
	period := int64(end.Sub(start) / time.Second)

	num := new(big.Int).Mul(big.NewInt(in.UnitAmountMinor), big.NewInt(in.SeatsAdded))
	num.Mul(num, big.NewInt(remaining))
	exact := new(big.Rat).SetFrac(num, big.NewInt(period))

	res := ProrationResult{
		UnitAmountMinor:  in.UnitAmountMinor,
		RemainingSeconds: remaining,
		PeriodSeconds:    period,
		AmountUnrounded:  decimalHalfUp(exact, 12),
		RoundedMinor:     roundHalfUp(exact),
	}
	res.AmountMinor = res.RoundedMinor

	if res.RoundedMinor > 0 && res.RoundedMinor < in.MinChargeMinor {
		switch in.Policy {
		case FreeUntilRenewal:
			res.DeferredAmountMinor = res.RoundedMinor
			res.DeferralReason = DeferralBelowProviderMinimum
			res.AmountMinor = 0
		case ChargeMinimum:
			res.AmountMinor = in.MinChargeMinor
		}
	}
	return res, nil
}

// roundHalfUp rounds a non-negative rational to the nearest integer, halves up.
func roundHalfUp(r *big.Rat) int64 {
	// floor((2n + d) / 2d)
	n, d := r.Num(), r.Denom()
	twoN := new(big.Int).Lsh(n, 1)
	twoN.Add(twoN, d)
	twoD := new(big.Int).Lsh(d, 1)
	return new(big.Int).Quo(twoN, twoD).Int64()
}

// decimalHalfUp formats a non-negative rational with exactly places decimals,
// rounding half-up at the last place.
func decimalHalfUp(r *big.Rat, places int) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(scale))
	n, d := scaled.Num(), scaled.Denom()
	twoN := new(big.Int).Lsh(n, 1)
	twoN.Add(twoN, d)
	units := new(big.Int).Quo(twoN, new(big.Int).Lsh(d, 1))
	intPart, frac := new(big.Int).QuoRem(units, scale, new(big.Int))
	fs := frac.String()
	for len(fs) < places {
		fs = "0" + fs
	}
	return intPart.String() + "." + fs
}
