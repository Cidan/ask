package memory

import (
	"math"
	"time"
)

// Weight tunables. Weight is the running importance of a concept: it
// decays exponentially with disuse and is bumped whenever the concept is
// surfaced (implicit) or judged useful by the model (explicit).
const (
	WeightInitial = 1.0
	WeightCap     = 5.0
	WeightFloor   = 0.05

	// ImplicitBump is applied to every concept a recall returns.
	ImplicitBump = 0.05
	// ExplicitBump is the reinforce/demote magnitude; positive bumps are
	// log-dampened toward WeightCap so repeated calls cannot pin a concept.
	ExplicitBump = 0.5
	// RefractoryPeriod drops a second explicit bump on the same concept
	// inside the window, so an implicit recall bump followed by an
	// immediate reinforce is not double-counted.
	RefractoryPeriod = 60 * time.Second

	ConceptHalfLife = 100 * 24 * time.Hour
	TopicHalfLife   = 60 * 24 * time.Hour

	// Reranking: score = sim^RerankSimExp * weight^RerankWeightExp, then
	// TopicBoost for candidates in the turn's topic.
	RerankSimExp    = 1.0
	RerankWeightExp = 0.5
	TopicBoost      = 1.15

	// CandidateOversample is how many raw vector hits are pulled before
	// reranking; MaxDistance is the cosine-distance relevance floor.
	CandidateOversample = 50
	MaxDistance         = 0.4
)

// decayedWeight applies exponential decay to w over dt with the given
// half-life, never below WeightFloor.
func decayedWeight(w float64, dt time.Duration, halfLife time.Duration) float64 {
	if dt <= 0 || halfLife <= 0 {
		return clampWeight(w)
	}
	lambda := math.Ln2 / halfLife.Seconds()
	return clampWeight(w * math.Exp(-lambda*dt.Seconds()))
}

func clampWeight(w float64) float64 {
	if w < WeightFloor {
		return WeightFloor
	}
	if w > WeightCap {
		return WeightCap
	}
	return w
}

// bumpWeight adds delta to an already-decayed weight. Positive explicit
// deltas shrink as the weight approaches the cap; negative deltas apply
// in full and clamp at the floor.
func bumpWeight(decayed, delta float64, dampen bool) float64 {
	if delta > 0 && dampen {
		delta *= 1 - decayed/WeightCap
	}
	return clampWeight(decayed + delta)
}

// rerankScore combines vector similarity with decayed weight.
func rerankScore(distance float32, weight float64) float64 {
	sim := 1 - float64(distance)
	if sim < 0 {
		sim = 0
	}
	return math.Pow(sim, RerankSimExp) * math.Pow(weight, RerankWeightExp)
}
