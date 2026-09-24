package jev

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Content fills the writable parts of a question: its instructions, a choice
// option's description, a score level, or either side of a noul. Plain text
// covers most of them; anything that encodes as a JSON object or array covers
// the rest. A nil Content reaches the API as null, meaning "left undescribed".
type Content = any

// Kind is the wire type of a question and of its answer.
type Kind string

// The question kinds the System One API accepts.
const (
	KindNoul   Kind = "noul"
	KindChoice Kind = "choice"
	KindScore  Kind = "score"
)

// Text renders a [Content] for logs and messages: a string as it is, nil as
// an empty string, and anything else as compact JSON.
func Text(c Content) string {
	switch v := c.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	b, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		return fmt.Sprintf("%v", c)
	}
	return string(b)
}

// Spec is one question as it is sent over the wire. The typed constructors
// [Noul], [Choice], [OneOf], and [Score] produce it; [Raw] sends one as
// given. Providers and fakes read it.
type Spec struct {
	Type Kind `json:"type"`
	// Instructions is optional, as in the official SDKs: a choice or score
	// whose criteria say everything can leave it nil.
	Instructions Content `json:"instructions,omitzero"`
	// Criteria is *NoulCriteria for a noul, [ChoiceCriteria] for a choice (a
	// nil description is sent as null), and []Content for a score.
	Criteria any `json:"criteria,omitzero"`
	// Extra carries question fields this package does not model. They are
	// marshaled alongside the modeled fields; "type", "instructions", and
	// "criteria" are reserved and rejected before sending.
	Extra map[string]any `json:",embed"`
}

// ChoiceCriterion is one option of a choice as it is sent: its name and
// description.
type ChoiceCriterion struct {
	Name        string
	Description Content
}

// ChoiceCriteria is the criteria of a choice, in the order the options were
// declared. It encodes as a JSON object whose keys keep that order, which a
// map cannot do, so the model reads the options as they were written.
type ChoiceCriteria []ChoiceCriterion

// MarshalJSONTo implements [json.MarshalerTo].
func (c ChoiceCriteria) MarshalJSONTo(enc *jsontext.Encoder) error {
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, o := range c {
		if err := enc.WriteToken(jsontext.String(o.Name)); err != nil {
			return err
		}
		if err := json.MarshalEncode(enc, o.Description); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// ChoiceOptions returns the option names of a choice, or nil for other
// kinds. Names come in declared order for [ChoiceCriteria] and sorted for a
// map, such as the criteria of a Spec decoded from JSON.
func (s Spec) ChoiceOptions() []string {
	switch c := s.Criteria.(type) {
	case ChoiceCriteria:
		names := make([]string, len(c))
		for i, o := range c {
			names[i] = o.Name
		}
		return names
	case map[string]Content:
		return slices.Sorted(maps.Keys(c))
	case map[string]string:
		return slices.Sorted(maps.Keys(c))
	}
	return nil
}

// ScoreLevels reports how many levels a score has, or 0 for other kinds.
func (s Spec) ScoreLevels() int {
	if c, ok := s.Criteria.([]Content); ok {
		return len(c)
	}
	return 0
}

// NoulCriteria spells out the two outcomes of a [Noul]. Either side may be
// nil, and either may be structured [Content].
type NoulCriteria struct {
	// True describes the outcome the probability climbs towards.
	True Content `json:"true,omitzero"`
	// False describes the outcome it falls towards.
	False Content `json:"false,omitzero"`
}

// Question is a typed question whose answer decodes to A. Build one with
// [Noul], [Choice], [OneOf], [Score], or [Raw]. Questions are immutable
// values meant to be declared once, typically at package level.
//
// The interface is sealed: a question built outside this package could not
// be answered by the API, so the compiler prevents it. [Raw] is the
// deliberate way around the typed constructors.
type Question[A any] interface {
	spec() (Spec, error)
	decode(raw RawAnswer) (A, error)
}

// invalidQ is a question that failed to build. It is reported by
// [Batch.Run] and [Handle.Get] instead of panicking in a constructor.
type invalidQ[A any] struct{ err error }

func (q invalidQ[A]) spec() (Spec, error) { return Spec{}, q.err }
func (q invalidQ[A]) decode(RawAnswer) (A, error) {
	var zero A
	return zero, q.err
}

// ---------------------------------------------------------------- raw

type rawQ struct{ raw Spec }

// Raw sends spec exactly as given and hands the answer back undecoded, so a
// question kind or field that lands in the API before a release of this
// package does is still reachable. Nothing is validated or narrowed; reach
// for a typed constructor whenever one fits.
func Raw(spec Spec) Question[RawAnswer] { return rawQ{spec} }

func (q rawQ) spec() (Spec, error) {
	if q.raw.Type == "" {
		return Spec{}, invalid("raw question needs a non-empty type")
	}
	for _, key := range []string{"type", "instructions", "criteria"} {
		if _, clash := q.raw.Extra[key]; clash {
			return Spec{}, invalid("raw question: %q is a reserved field, set it on Spec directly", key)
		}
	}
	return q.raw, nil
}

func (q rawQ) decode(raw RawAnswer) (RawAnswer, error) { return raw, nil }

// ---------------------------------------------------------------- noul

type noulQ struct {
	instructions Content
	criteria     *NoulCriteria
}

// Noul settles a question with two outcomes, answering with how likely the
// affirmative one is. Pass one [NoulCriteria] to spell the outcomes out;
// doing so usually moves a hedged number towards a decisive one. The API
// wants instructions, criteria, or both.
//
// When several labels can be true at once, ask a separate Noul for each
// rather than forcing a [Choice] between them. Read 0.5 as "the model cannot
// separate yes from no", not as "somewhere in the middle"; a middle is what
// a [Score] is for.
func Noul(instructions Content, criteria ...NoulCriteria) Question[NoulAnswer] {
	if len(criteria) > 1 {
		return invalidQ[NoulAnswer]{invalid("noul takes at most one NoulCriteria, got %d", len(criteria))}
	}
	q := noulQ{instructions: instructions}
	if len(criteria) == 1 {
		q.criteria = new(criteria[0])
	}
	return q
}

func (q noulQ) spec() (Spec, error) {
	s := Spec{Type: KindNoul, Instructions: q.instructions}
	if q.criteria != nil && (q.criteria.True != nil || q.criteria.False != nil) {
		s.Criteria = q.criteria
	}
	if s.Instructions == nil && s.Criteria == nil {
		return Spec{}, invalid("noul needs instructions or criteria")
	}
	return s, nil
}

func (q noulQ) decode(raw RawAnswer) (NoulAnswer, error) {
	if raw.Type != KindNoul {
		return NoulAnswer{}, malformed("expected a %s answer, got %q", KindNoul, raw.Type)
	}
	if raw.Noul == nil || !isProbability(*raw.Noul) {
		return NoulAnswer{}, malformed("noul answer has no valid probability")
	}
	return NoulAnswer{P: *raw.Noul}, nil
}

// ---------------------------------------------------------------- choice

// ChoiceOption is one option of a [Choice]: a value of the caller's string
// type and an optional description.
type ChoiceOption[T ~string] struct {
	Value       T
	Description Content
}

// Opt builds a [ChoiceOption]. T is inferred from value, so passing a
// constant of your own string type is what makes the whole choice typed. A
// nil description leaves the option to stand on its name.
func Opt[T ~string](value T, description Content) ChoiceOption[T] {
	return ChoiceOption[T]{Value: value, Description: description}
}

type choiceQ[T ~string] struct {
	instructions Content
	options      []ChoiceOption[T]
}

// Choice settles on exactly one of the options and reports how the
// probability was spread over all of them. The set is a closed world, so
// offer somewhere for an input to land that none of the real options
// describe.
func Choice[T ~string](instructions Content, options ...ChoiceOption[T]) Question[ChoiceAnswer[T]] {
	return choiceQ[T]{instructions, slices.Clone(options)}
}

// OneOf is [Choice] without descriptions, for options that need no
// explaining.
func OneOf[T ~string](instructions Content, values ...T) Question[ChoiceAnswer[T]] {
	options := make([]ChoiceOption[T], len(values))
	for i, v := range values {
		options[i] = ChoiceOption[T]{Value: v}
	}
	return choiceQ[T]{instructions, options}
}

func (q choiceQ[T]) spec() (Spec, error) {
	if len(q.options) == 0 {
		return Spec{}, invalid("choice %q has no options", Text(q.instructions))
	}
	criteria := make(ChoiceCriteria, 0, len(q.options))
	seen := make(map[string]bool, len(q.options))
	for _, o := range q.options {
		name := string(o.Value)
		if strings.TrimSpace(name) == "" {
			return Spec{}, invalid("choice %q has an empty option", Text(q.instructions))
		}
		if seen[name] {
			return Spec{}, invalid("choice %q has duplicate option %q", Text(q.instructions), name)
		}
		seen[name] = true
		// An option left undescribed is null on the wire; "" would read as a
		// description that happens to be empty.
		description := o.Description
		if s, ok := description.(string); ok && s == "" {
			description = nil
		}
		criteria = append(criteria, ChoiceCriterion{Name: name, Description: description})
	}
	return Spec{Type: KindChoice, Instructions: q.instructions, Criteria: criteria}, nil
}

func (q choiceQ[T]) decode(raw RawAnswer) (ChoiceAnswer[T], error) {
	var zero ChoiceAnswer[T]
	if raw.Type != KindChoice {
		return zero, malformed("expected a %s answer, got %q", KindChoice, raw.Type)
	}
	if raw.Choice == nil {
		return zero, malformed("choice answer has no choice")
	}
	valid := make(map[string]T, len(q.options))
	probs := make(map[T]float64, len(q.options))
	for _, o := range q.options {
		valid[string(o.Value)] = o.Value
		probs[o.Value] = 0
	}
	selected, ok := valid[*raw.Choice]
	if !ok {
		return zero, malformed("choice answer %q is not one of the options", *raw.Choice)
	}
	for name, p := range raw.Probabilities {
		// A name that is not an option cannot be expressed as T. It is left
		// out rather than failing the answer; the raw answer in
		// [Response.Answers] still carries it.
		v, ok := valid[name]
		if !ok {
			continue
		}
		if !isProbability(p) {
			return zero, malformed("invalid probability %v for option %q", p, name)
		}
		probs[v] = p
	}
	return ChoiceAnswer[T]{Value: selected, Probs: probs, Confidence: deref(raw.Confidence)}, nil
}

// ---------------------------------------------------------------- score

type scoreQ struct {
	instructions Content
	levels       []Content
}

// Score places the state on a scale whose points you describe, lowest
// first. Write each level as a situation someone could recognise, not as a
// grade: the answer is a probability-weighted position along the levels, so
// it may come to rest between two of them, and that reading is the useful
// part rather than noise to round away.
func Score(instructions Content, levels ...Content) Question[ScoreAnswer] {
	return scoreQ{instructions, slices.Clone(levels)}
}

func (q scoreQ) spec() (Spec, error) {
	if len(q.levels) == 0 {
		return Spec{}, invalid("score %q has no levels", Text(q.instructions))
	}
	return Spec{Type: KindScore, Instructions: q.instructions, Criteria: slices.Clone(q.levels)}, nil
}

func (q scoreQ) decode(raw RawAnswer) (ScoreAnswer, error) {
	if raw.Type != KindScore {
		return ScoreAnswer{}, malformed("expected a %s answer, got %q", KindScore, raw.Type)
	}
	n := len(q.levels)
	if raw.Score == nil || raw.Score != raw.Score || *raw.Score < 0 || *raw.Score > float64(n-1) {
		return ScoreAnswer{}, malformed("score answer is missing or outside 0..%d", n-1)
	}
	probs := make([]float64, n)
	for key, p := range raw.Probabilities {
		// Keys that name no level are left out, as for a choice.
		i, err := strconv.Atoi(key)
		if err != nil || i < 0 || i >= n {
			continue
		}
		if !isProbability(p) {
			return ScoreAnswer{}, malformed("invalid probability %v for level %q", p, key)
		}
		probs[i] = p
	}
	return ScoreAnswer{
		Value:      *raw.Score,
		Probs:      probs,
		Levels:     slices.Clone(q.levels),
		Confidence: deref(raw.Confidence),
	}, nil
}

func isProbability(p float64) bool { return p == p && p >= 0 && p <= 1 }

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
