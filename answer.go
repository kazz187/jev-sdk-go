package jev

import (
	"cmp"
	"encoding/json/jsontext"
	"math"
	"slices"
)

// RawAnswer is an answer as it arrives over the wire, before it is narrowed
// to the type of its question. [Response.Answers] holds one per question;
// [Raw] questions return it directly.
type RawAnswer struct {
	Type          Kind               `json:"type"`
	Noul          *float64           `json:"noul,omitzero"`
	Choice        *string            `json:"choice,omitzero"`
	Score         *float64           `json:"score,omitzero"`
	Confidence    *float64           `json:"confidence,omitzero"`
	Probabilities map[string]float64 `json:"probabilities,omitzero"`
	// Legend maps each score level index to the description that was sent.
	// Values stay raw because a level may be structured [Content].
	Legend map[string]jsontext.Value `json:"legend,omitzero"`
	// Extra holds answer fields this package does not model.
	Extra map[string]jsontext.Value `json:",embed"`
}

// NoulAnswer is how likely the model finds a yes, from 0 to 1. A noul carries
// no separate confidence, because this number already is one.
type NoulAnswer struct {
	P float64
}

// Sure reports the answer and whether it clears threshold in either
// direction: (true, true) when P >= threshold, (false, true) when
// 1-P >= threshold, and (false, false) in the uncertain middle. Use a
// threshold above 0.5.
func (a NoulAnswer) Sure(threshold float64) (yes, ok bool) {
	switch {
	case a.P >= threshold:
		return true, true
	case 1-a.P >= threshold:
		return false, true
	}
	return false, false
}

// ChoiceAnswer is the selected option and the distribution over all of them.
type ChoiceAnswer[T ~string] struct {
	// Value is the most probable option.
	Value T
	// Probs holds a probability for every option, including zero ones.
	Probs map[T]float64
	// Confidence is the API's own field: how peaked the distribution is,
	// from 0 to 1. It is not a calibrated probability that the answer is
	// right; use Probs[Value] for that.
	Confidence float64
}

// Sure returns Value and whether its probability is at least threshold.
func (a ChoiceAnswer[T]) Sure(threshold float64) (T, bool) {
	return a.Value, a.Probs[a.Value] >= threshold
}

// Ranked returns the options from most to least probable, ties broken by
// name so the order is stable.
func (a ChoiceAnswer[T]) Ranked() []T {
	return slices.SortedFunc(func(yield func(T) bool) {
		for v := range a.Probs {
			if !yield(v) {
				return
			}
		}
	}, func(x, y T) int {
		return cmp.Or(cmp.Compare(a.Probs[y], a.Probs[x]), cmp.Compare(x, y))
	})
}

// ScoreAnswer is where a [Score] came to rest on its own scale.
type ScoreAnswer struct {
	// Value runs from 0 (the first level) to len(Levels)-1. It is the
	// probability-weighted position, so it usually falls between levels.
	Value float64
	// Probs holds one probability per level, lowest level first.
	Probs []float64
	// Levels are the level descriptions from the question, in order.
	Levels []Content
	// Confidence is the API's confidence; see [ChoiceAnswer.Confidence].
	Confidence float64
}

// Nearest returns the index and description of the level closest to Value.
func (a ScoreAnswer) Nearest() (int, Content) {
	if len(a.Levels) == 0 {
		return 0, nil
	}
	i := min(max(int(math.Round(a.Value)), 0), len(a.Levels)-1)
	return i, a.Levels[i]
}

// Likeliest returns the index and description of the level with the highest
// probability, the lowest on a tie. It differs from [ScoreAnswer.Nearest]
// when the distribution has two peaks.
func (a ScoreAnswer) Likeliest() (int, Content) {
	if len(a.Levels) == 0 {
		return 0, nil
	}
	best := 0
	for i, p := range a.Probs {
		if p > a.Probs[best] {
			best = i
		}
	}
	return best, a.Levels[best]
}
