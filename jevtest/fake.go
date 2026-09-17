// Package jevtest provides a rule-based fake [jev.Provider] for tests.
//
//	fake := jevtest.New().
//		On(jevtest.Instructions("spam"), jevtest.Yes(0.97)).
//		On(jevtest.Kind(jev.KindChoice), jevtest.Pick("refund", 0.9))
//	client, _ := jev.New(jev.WithProvider(fake))
//
// A question that matches no rule fails the request, so a missing rule shows
// up as a test failure instead of a silent default.
package jevtest

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/kazz187/jev-sdk-go"
)

// Matcher selects which questions a rule answers. It sees the request's
// state as JSON and the question's wire form.
type Matcher func(state jsontext.Value, q jev.Spec) bool

// Answer produces the raw answer for a matched question.
type Answer func(q jev.Spec) (jev.RawAnswer, error)

type rule struct {
	match  Matcher
	answer Answer
}

// Fake is a [jev.Provider] that answers from rules. It is safe for
// concurrent use.
type Fake struct {
	mu    sync.Mutex
	rules []rule
	calls []jev.Request
}

// New returns a Fake with no rules.
func New() *Fake { return &Fake{} }

// On adds a rule. Rules are tried in the order they were added.
func (f *Fake) On(m Matcher, a Answer) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, rule{m, a})
	return f
}

// Calls returns the requests received so far, most recent last.
func (f *Fake) Calls() []jev.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Evaluate implements [jev.Provider].
func (f *Fake) Evaluate(ctx context.Context, req *jev.Request) (*jev.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := json.Marshal(req.State, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("jevtest: encoding state: %w", err)
	}
	f.mu.Lock()
	f.calls = append(f.calls, *req)
	rules := slices.Clone(f.rules)
	f.mu.Unlock()

	answers := make(map[string]jev.RawAnswer, len(req.Questions))
	for name, q := range req.Questions {
		i := slices.IndexFunc(rules, func(r rule) bool { return r.match(state, q) })
		if i < 0 {
			return nil, fmt.Errorf("jevtest: no rule matches question %q (%s)", name, jev.Text(q.Instructions))
		}
		a, err := rules[i].answer(q)
		if err != nil {
			return nil, fmt.Errorf("jevtest: question %q: %w", name, err)
		}
		answers[name] = a
	}
	return &jev.Response{RequestID: "req_jevtest", Model: "jevtest", Answers: answers}, nil
}

// Any matches every question.
func Any() Matcher { return func(jsontext.Value, jev.Spec) bool { return true } }

// Instructions matches questions whose instructions contain substr.
// Structured instructions are rendered with [jev.Text] first.
func Instructions(substr string) Matcher {
	return func(_ jsontext.Value, q jev.Spec) bool {
		return strings.Contains(jev.Text(q.Instructions), substr)
	}
}

// Kind matches questions of one kind.
func Kind(k jev.Kind) Matcher {
	return func(_ jsontext.Value, q jev.Spec) bool { return q.Type == k }
}

// Option matches choice questions that offer the given option.
func Option(name string) Matcher {
	return func(_ jsontext.Value, q jev.Spec) bool { return slices.Contains(q.ChoiceOptions(), name) }
}

// State matches requests whose JSON-encoded state contains substr.
func State(substr string) Matcher {
	return func(s jsontext.Value, _ jev.Spec) bool { return strings.Contains(string(s), substr) }
}

// All matches when every matcher matches.
func All(ms ...Matcher) Matcher {
	return func(s jsontext.Value, q jev.Spec) bool {
		for _, m := range ms {
			if !m(s, q) {
				return false
			}
		}
		return true
	}
}

// Not inverts a matcher.
func Not(m Matcher) Matcher {
	return func(s jsontext.Value, q jev.Spec) bool { return !m(s, q) }
}

// Yes answers a noul question with probability p of yes.
func Yes(p float64) Answer {
	return func(q jev.Spec) (jev.RawAnswer, error) {
		if q.Type != jev.KindNoul {
			return jev.RawAnswer{}, fmt.Errorf("Yes used on a %s question", q.Type)
		}
		return jev.RawAnswer{Type: jev.KindNoul, Noul: new(p)}, nil
	}
}

// Pick answers a choice with option at probability p and spreads the rest
// evenly over the other options.
func Pick(option string, p float64) Answer {
	return func(q jev.Spec) (jev.RawAnswer, error) {
		if q.Type != jev.KindChoice {
			return jev.RawAnswer{}, fmt.Errorf("Pick used on a %s question", q.Type)
		}
		options := q.ChoiceOptions()
		if !slices.Contains(options, option) {
			return jev.RawAnswer{}, fmt.Errorf("Pick(%q): not an option of %v", option, options)
		}
		probs := make(map[string]float64, len(options))
		for _, o := range options {
			probs[o] = spread(p, len(options))
		}
		probs[option] = p
		return jev.RawAnswer{Type: jev.KindChoice, Choice: new(option), Probabilities: probs, Confidence: new(p)}, nil
	}
}

// Level answers a score with level i at probability p and spreads the rest
// evenly over the other levels. The score value is the weighted mean.
func Level(i int, p float64) Answer {
	return func(q jev.Spec) (jev.RawAnswer, error) {
		if q.Type != jev.KindScore {
			return jev.RawAnswer{}, fmt.Errorf("Level used on a %s question", q.Type)
		}
		n := q.ScoreLevels()
		if i < 0 || i >= n {
			return jev.RawAnswer{}, fmt.Errorf("Level(%d): score has %d levels", i, n)
		}
		probs := make(map[string]float64, n)
		legend := make(map[string]jsontext.Value, n)
		value := 0.0
		for l := range n {
			pl := spread(p, n)
			if l == i {
				pl = p
			}
			key := fmt.Sprint(l)
			probs[key] = pl
			legend[key], _ = json.Marshal(q.Criteria.([]jev.Content)[l])
			value += float64(l) * pl
		}
		return jev.RawAnswer{
			Type: jev.KindScore, Score: new(value), Legend: legend, Probabilities: probs, Confidence: new(p),
		}, nil
	}
}

// Fail makes matched questions fail the whole request with err.
func Fail(err error) Answer {
	if err == nil {
		err = errors.New("jevtest: injected failure")
	}
	return func(jev.Spec) (jev.RawAnswer, error) { return jev.RawAnswer{}, err }
}

// spread is the share each of the other n-1 options gets when one has p.
func spread(p float64, n int) float64 {
	if n <= 1 {
		return 0
	}
	return (1 - p) / float64(n-1)
}
