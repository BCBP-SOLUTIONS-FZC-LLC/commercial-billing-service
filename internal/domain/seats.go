package domain

// Seat rules of HLD §8.5.1 and LLD §8.6.

// MaxLicensedSeats is the upper bound of a seat item quantity (§4.2.5).
const MaxLicensedSeats = 100000

// SeatsInUse is I-11's count of seats a tenant uses: active users plus
// pending invitations (§8.6.2).
func SeatsInUse(activeUsers, pendingInvitations int) int {
	return activeUsers + pendingInvitations
}

// RenewalSeatCount is the quantity a scheduled reduction renews at:
// max(target_seats, seats_in_use) (BIL-SEAT-3). A reduction never drops
// below what the tenant uses.
func RenewalSeatCount(targetSeats, seatsInUse int) int {
	if seatsInUse > targetSeats {
		return seatsInUse
	}
	return targetSeats
}

// RenewalTopUp returns how many seats a renewal must add when members were
// added inside the lead window: seats_in_use_now − renewal_seats, or 0
// (§8.6.3). After the top-up the licensed count is never below I-11's count
// (BIL-SEAT-3).
func RenewalTopUp(renewalSeats, seatsInUseNow int) int {
	if seatsInUseNow > renewalSeats {
		return seatsInUseNow - renewalSeats
	}
	return 0
}

// FinalRenewalSeats is the licensed count after renewal and any top-up.
func FinalRenewalSeats(renewalSeats, seatsInUseNow int) int {
	return renewalSeats + RenewalTopUp(renewalSeats, seatsInUseNow)
}

// ValidReductionTarget reports whether a scheduled reduction target is
// below the current count (else 422 TARGET_NOT_BELOW_CURRENT) and at least 1.
func ValidReductionTarget(target, currentSeats int) bool {
	return target >= 1 && target < currentSeats
}
