package jev_test

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/kazz187/jev-sdk-go"
)

type Team string

const (
	Billing   Team = "billing"
	Technical Team = "technical"
	Other     Team = "other"
)

// capture is a provider that records the request and answers from a canned
// response.
func capture(t *testing.T, answers map[string]jev.RawAnswer) (*jev.Client, *jev.Request) {
	t.Helper()
	var got jev.Request
	client, err := jev.New(jev.WithProvider(jev.ProviderFunc(func(_ context.Context, r *jev.Request) (*jev.Response, error) {
		got = *r
		return &jev.Response{Model: "jev-test", Answers: answers}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	return client, &got
}

func wire(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWireFormat(t *testing.T) {
	client, got := capture(t, map[string]jev.RawAnswer{
		"spam":  {Type: jev.KindNoul, Noul: new(0.9)},
		"team":  {Type: jev.KindChoice, Choice: new("billing"), Probabilities: map[string]float64{"billing": 0.8, "other": 0.2}, Confidence: new(0.6)},
		"anger": {Type: jev.KindScore, Score: new(1.5), Probabilities: map[string]float64{"0": 0, "1": 0.5, "2": 0.5}, Confidence: new(0.5)},
		"raw":   {Type: "rank", Extra: map[string]jsontext.Value{"order": jsontext.Value(`["a","b"]`)}},
	})
	b := client.Batch(map[string]any{"text": "I was charged twice"}).Model("jev-1.13").Extra(map[string]any{"trace": true})
	spam := b.Add("spam", jev.Noul("Is this spam?", jev.NoulCriteria{True: "advertising", False: map[string]string{"kind": "genuine"}}))
	team := b.Add("team", jev.Choice("Which team?", jev.Opt(Billing, "money"), jev.Opt(Technical, ""), jev.Opt(Other, nil)))
	anger := b.Add("anger", jev.Score(nil, "Calm", map[string]any{"level": "Annoyed"}, "Furious"))
	raw := b.Add("raw", jev.Raw(jev.Spec{Type: "rank", Instructions: "Rank these", Extra: map[string]any{"beam": 4}}))

	resp, err := b.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"jev-1.13","state":{"text":"I was charged twice"},"questions":{` +
		`"anger":{"type":"score","criteria":["Calm",{"level":"Annoyed"},"Furious"]},` +
		`"raw":{"type":"rank","instructions":"Rank these","beam":4},` +
		`"spam":{"type":"noul","instructions":"Is this spam?","criteria":{"true":"advertising","false":{"kind":"genuine"}}},` +
		`"team":{"type":"choice","instructions":"Which team?","criteria":{"billing":"money","technical":null,"other":null}}` +
		`},"trace":true}`
	if got := wire(t, got); got != want {
		t.Errorf("wire format\n got: %s\nwant: %s", got, want)
	}
	if resp.Model != "jev-test" || len(resp.Answers) != 4 {
		t.Errorf("response = %+v", resp)
	}

	if s, err := spam.Get(); err != nil || s.P != 0.9 {
		t.Errorf("spam = %v, %v", s, err)
	}
	tm, err := team.Get()
	if err != nil || tm.Value != Billing || tm.Probs[Technical] != 0 || tm.Probs[Billing] != 0.8 || tm.Confidence != 0.6 {
		t.Errorf("team = %+v, %v", tm, err)
	}
	if got := tm.Ranked(); !equal(got, []Team{Billing, Other, Technical}) {
		t.Errorf("Ranked = %v", got)
	}
	a, err := anger.Get()
	if err != nil || a.Value != 1.5 || !equal(a.Probs, []float64{0, 0.5, 0.5}) || len(a.Levels) != 3 {
		t.Errorf("anger = %+v, %v", a, err)
	}
	if i, _ := a.Nearest(); i != 2 {
		t.Errorf("Nearest = %d, want 2", i)
	}
	if i, level := a.Likeliest(); i != 1 || jev.Text(level) != `{"level":"Annoyed"}` {
		t.Errorf("Likeliest = %d %v", i, level)
	}
	r, err := raw.Get()
	if err != nil || r.Type != "rank" || string(r.Extra["order"]) != `["a","b"]` {
		t.Errorf("raw = %+v, %v", r, err)
	}
}

func errOf[A any](h *jev.Handle[A]) error {
	_, err := h.Get()
	return err
}

func equal[S ~[]E, E comparable](a, b S) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestInvalidQuestionsAreReportedNotPanicked(t *testing.T) {
	client, _ := capture(t, nil)
	cases := map[string]func(b *jev.Batch) error{
		"empty choice":     func(b *jev.Batch) error { return errOf(b.Add("q", jev.OneOf[Team]("Which?"))) },
		"duplicate option": func(b *jev.Batch) error { return errOf(b.Add("q", jev.OneOf("Which?", Billing, Billing))) },
		"blank option":     func(b *jev.Batch) error { return errOf(b.Add("q", jev.OneOf("Which?", Team(" ")))) },
		"empty score":      func(b *jev.Batch) error { return errOf(b.Add("q", jev.Score("How?"))) },
		"noul two criteria": func(b *jev.Batch) error {
			return errOf(b.Add("q", jev.Noul("?", jev.NoulCriteria{}, jev.NoulCriteria{})))
		},
		"noul nothing":     func(b *jev.Batch) error { return errOf(b.Add("q", jev.Noul(nil))) },
		"raw without type": func(b *jev.Batch) error { return errOf(b.Add("q", jev.Raw(jev.Spec{}))) },
		"raw reserved extra": func(b *jev.Batch) error {
			return errOf(b.Add("q", jev.Raw(jev.Spec{Type: "x", Extra: map[string]any{"type": 1}})))
		},
		"empty name":   func(b *jev.Batch) error { return errOf(b.Add("", jev.Noul("?"))) },
		"nil question": func(b *jev.Batch) error { return errOf(b.Add("q", jev.Question[jev.NoulAnswer](nil))) },
		"duplicate name": func(b *jev.Batch) error {
			b.Add("q", jev.Noul("?"))
			return errOf(b.Add("q", jev.Noul("?")))
		},
	}
	for name, add := range cases {
		t.Run(name, func(t *testing.T) {
			b := client.Batch("s")
			if err := add(b); !errors.Is(err, jev.ErrInvalidRequest) {
				t.Errorf("Get() = %v, want ErrInvalidRequest", err)
			}
			if _, err := b.Run(t.Context()); !errors.Is(err, jev.ErrInvalidRequest) {
				t.Errorf("Run() = %v, want ErrInvalidRequest", err)
			}
		})
	}
	t.Run("no questions", func(t *testing.T) {
		if _, err := client.Batch("s").Run(t.Context()); !errors.Is(err, jev.ErrInvalidRequest) {
			t.Errorf("Run() = %v", err)
		}
	})
	t.Run("reserved extra", func(t *testing.T) {
		b := client.Batch("s").Extra(map[string]any{"model": "x"})
		b.Add("q", jev.Noul("?"))
		if _, err := b.Run(t.Context()); !errors.Is(err, jev.ErrInvalidRequest) {
			t.Errorf("Run() = %v", err)
		}
	})
}

func TestMalformedAnswersFailOnlyTheirHandle(t *testing.T) {
	client, _ := capture(t, map[string]jev.RawAnswer{
		"ok":         {Type: jev.KindNoul, Noul: new(0.2)},
		"wrong kind": {Type: jev.KindScore, Score: new(1.0)},
		"bad option": {Type: jev.KindChoice, Choice: new("nope"), Probabilities: map[string]float64{"billing": 1}},
		"bad prob":   {Type: jev.KindChoice, Choice: new("billing"), Probabilities: map[string]float64{"billing": 1.5}},
		"bad level":  {Type: jev.KindScore, Score: new(0.5), Probabilities: map[string]float64{"1": -0.1}},
		"range":      {Type: jev.KindScore, Score: new(9.0), Probabilities: map[string]float64{"0": 1}},
		"no noul":    {Type: jev.KindNoul},
	})
	b := client.Batch("s")
	ok := b.Add("ok", jev.Noul("?"))
	handles := map[string]*jev.Handle[jev.ChoiceAnswer[Team]]{
		"bad option": b.Add("bad option", jev.OneOf("?", Billing)),
		"bad prob":   b.Add("bad prob", jev.OneOf("?", Billing)),
	}
	wrongKind := b.Add("wrong kind", jev.Noul("?"))
	badLevel := b.Add("bad level", jev.Score("?", "a", "b"))
	outOfRange := b.Add("range", jev.Score("?", "a", "b"))
	noNoul := b.Add("no noul", jev.Noul("?"))
	missing := b.Add("missing", jev.Noul("?"))

	resp, err := b.Run(t.Context())
	if !errors.Is(err, jev.ErrMalformedAnswer) {
		t.Fatalf("Run() = %v, want ErrMalformedAnswer", err)
	}
	if resp == nil || len(resp.Answers) != 7 {
		t.Fatalf("response should still be returned, got %+v", resp)
	}
	if a, err := ok.Get(); err != nil || a.P != 0.2 {
		t.Errorf("ok = %v, %v", a, err)
	}
	for name, h := range handles {
		if _, err := h.Get(); !errors.Is(err, jev.ErrMalformedAnswer) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: Get() = %v", name, err)
		}
	}
	for name, get := range map[string]func() error{
		"wrong kind": func() error { return errOf(wrongKind) }, "bad level": func() error { return errOf(badLevel) },
		"range": func() error { return errOf(outOfRange) }, "no noul": func() error { return errOf(noNoul) },
		"missing": func() error { return errOf(missing) },
	} {
		if err := get(); !errors.Is(err, jev.ErrMalformedAnswer) {
			t.Errorf("%s: Get() = %v", name, err)
		}
	}
}

func TestUnknownProbabilitiesAreLeftOut(t *testing.T) {
	client, _ := capture(t, map[string]jev.RawAnswer{
		"team": {Type: jev.KindChoice, Choice: new("billing"), Confidence: new(0.7),
			Probabilities: map[string]float64{"billing": 0.8, "other": 0.1, "abstain": 0.1}},
		"anger": {Type: jev.KindScore, Score: new(0.5),
			Probabilities: map[string]float64{"0": 0.5, "1": 0.5, "7": 0.2, "high": 0.1}},
	})
	b := client.Batch("s")
	team := b.Add("team", jev.OneOf("?", Billing, Other))
	anger := b.Add("anger", jev.Score("?", "calm", "angry"))
	resp, err := b.Run(t.Context())
	if err != nil {
		t.Fatalf("Run() = %v, want unknown keys to be tolerated", err)
	}
	tm, err := team.Get()
	if err != nil || tm.Value != Billing || len(tm.Probs) != 2 || tm.Probs[Billing] != 0.8 || tm.Probs[Other] != 0.1 {
		t.Errorf("team = %+v, %v", tm, err)
	}
	if a, err := anger.Get(); err != nil || !equal(a.Probs, []float64{0.5, 0.5}) {
		t.Errorf("anger = %+v, %v", a, err)
	}
	// The raw answer keeps what the typed one leaves out.
	if resp.Answers["team"].Probabilities["abstain"] != 0.1 || resp.Answers["anger"].Probabilities["7"] != 0.2 {
		t.Errorf("raw answers = %+v", resp.Answers)
	}
}

func TestHandleLifecycle(t *testing.T) {
	client, _ := capture(t, map[string]jev.RawAnswer{"q": {Type: jev.KindNoul, Noul: new(1.0)}})
	b := client.Batch("s")
	h := b.Add("q", jev.Noul("?"))
	if _, err := h.Get(); !errors.Is(err, jev.ErrNotRun) {
		t.Errorf("before Run: %v", err)
	}
	if _, err := b.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(t.Context()); !errors.Is(err, jev.ErrBatchUsed) {
		t.Errorf("second Run: %v", err)
	}
	if _, err := b.Add("late", jev.Noul("?")).Get(); !errors.Is(err, jev.ErrBatchUsed) {
		t.Errorf("Add after Run: %v", err)
	}
	if h.Name() != "q" || b.Len() != 1 {
		t.Errorf("Name = %q, Len = %d", h.Name(), b.Len())
	}
}

func TestProviderErrorReachesEveryHandle(t *testing.T) {
	boom := errors.New("boom")
	client, err := jev.New(jev.WithProvider(jev.ProviderFunc(func(context.Context, *jev.Request) (*jev.Response, error) {
		return nil, boom
	})))
	if err != nil {
		t.Fatal(err)
	}
	b := client.Batch("s")
	h := b.Add("q", jev.Noul("?"))
	if _, err := b.Run(t.Context()); !errors.Is(err, boom) {
		t.Errorf("Run() = %v", err)
	}
	if _, err := h.Get(); !errors.Is(err, boom) {
		t.Errorf("Get() = %v", err)
	}
	if _, err := client.Ask(t.Context(), "s", jev.Noul("?")); !errors.Is(err, boom) {
		t.Errorf("Ask() = %v", err)
	}
}

func TestAnswerHelpers(t *testing.T) {
	yes, ok := jev.NoulAnswer{P: 0.95}.Sure(0.9)
	no, ok2 := jev.NoulAnswer{P: 0.05}.Sure(0.9)
	_, ok3 := jev.NoulAnswer{P: 0.5}.Sure(0.9)
	if !yes || !ok || no || !ok2 || ok3 {
		t.Errorf("Sure: %v %v %v %v %v", yes, ok, no, ok2, ok3)
	}
	c := jev.ChoiceAnswer[Team]{Value: Billing, Probs: map[Team]float64{Billing: 0.7, Other: 0.3}}
	if v, sure := c.Sure(0.8); v != Billing || sure {
		t.Errorf("Sure = %v %v", v, sure)
	}
	s := jev.ScoreAnswer{Value: 2.4, Probs: []float64{0.5, 0, 0.5}, Levels: []jev.Content{"a", "b", "c"}}
	if i, l := s.Nearest(); i != 2 || l != "c" {
		t.Errorf("Nearest = %d %v", i, l)
	}
	if i, _ := s.Likeliest(); i != 0 {
		t.Errorf("Likeliest = %d, want lowest on tie", i)
	}
	if i, l := (jev.ScoreAnswer{}).Nearest(); i != 0 || l != nil {
		t.Errorf("empty Nearest = %d %v", i, l)
	}
	if jev.Text(nil) != "" || jev.Text("x") != "x" || jev.Text([]int{1}) != "[1]" {
		t.Error("Text")
	}
}

func TestSpecInspection(t *testing.T) {
	var spec jev.Spec
	if err := json.Unmarshal([]byte(`{"type":"choice","criteria":{"b":null,"a":"x"},"extra":1}`), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Type != jev.KindChoice || spec.Extra["extra"] != float64(1) {
		t.Errorf("spec = %+v", spec)
	}
	// Content is an alias of any, so decoded criteria are still readable.
	if got := spec.ChoiceOptions(); !equal(got, []string{"a", "b"}) {
		t.Errorf("ChoiceOptions = %v", got)
	}
}

func TestChoiceKeepsDeclaredOrder(t *testing.T) {
	client, got := capture(t, map[string]jev.RawAnswer{
		"q": {Type: jev.KindChoice, Choice: new("other"), Probabilities: map[string]float64{"other": 1}},
	})
	if _, err := client.Ask(t.Context(), "s", jev.OneOf("?", Technical, Other, Billing)); err != nil {
		t.Fatal(err)
	}
	spec := got.Questions["q"]
	if names := spec.ChoiceOptions(); !equal(names, []string{"technical", "other", "billing"}) {
		t.Errorf("ChoiceOptions = %v", names)
	}
	if b := wire(t, spec.Criteria); b != `{"technical":null,"other":null,"billing":null}` {
		t.Errorf("criteria = %s", b)
	}
	// A description may itself be structured; its own keys are encoded as usual.
	criteria := jev.ChoiceCriteria{{Name: "z", Description: map[string]any{"b": 1, "a": 2}}, {Name: "a"}}
	if b := wire(t, criteria); b != `{"z":{"a":2,"b":1},"a":null}` {
		t.Errorf("structured criteria = %s", b)
	}
}
