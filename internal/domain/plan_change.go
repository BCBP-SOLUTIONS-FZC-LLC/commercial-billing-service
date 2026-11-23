package domain

// PlanChangeDirection says whether a plan change is an upgrade or a downgrade.
type PlanChangeDirection string

// Directions.
const (
	DirectionUpgrade   PlanChangeDirection = "upgrade"
	DirectionDowngrade PlanChangeDirection = "downgrade"
	DirectionSame      PlanChangeDirection = "same"
)

// planRank orders the plans: starter < pro < enterprise (§3 plan_change.go).
var planRank = map[PlanCode]int{PlanStarter: 1, PlanPro: 2, PlanEnterprise: 3}

// PlanRank returns the plan's rank, 0 for an unknown plan.
func PlanRank(p PlanCode) int { return planRank[p] }

// DirectionOf returns the direction of from → to. An upgrade takes effect
// when paid; a downgrade is scheduled for period end after an I-18 check
// (§8.4). same answers 409 SAME_PLAN.
func DirectionOf(from, to PlanCode) PlanChangeDirection {
	switch rf, rt := PlanRank(from), PlanRank(to); {
	case rt > rf:
		return DirectionUpgrade
	case rt < rf:
		return DirectionDowngrade
	}
	return DirectionSame
}
