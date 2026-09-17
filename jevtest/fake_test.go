package jevtest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kazz187/jev-sdk-go"
	"github.com/kazz187/jev-sdk-go/jevtest"
)

type Intent string

const (
	Refund Intent = "refund"
	Other  Intent = "other"
)

func TestFakeAnswersFromRules(t *testing.T) {
	fake := jevtest.New().
		On(jevtest.Instructions("spam"), jevtest.Yes(0.02)).
		On(jevtest.All(jevtest.Kind(jev.KindChoice), jevtest.Option("refund")), jevtest.Pick("refund", 0.9)).
		On(jevtest.State("broken"), jevtest.Level(2, 0.8))
	client, err := jev.New(jev.WithProvider(fake))
	if err != nil {
		t.Fatal(err)
	}

	b := client.Batch("the export is broken")
	spam := b.Add("spam", jev.Noul("Is this spam?"))
	intent := b.Add("intent", jev.Choice("What does the customer want?", jev.Opt(Refund, "money back"), jev.Opt(Other, nil)))
	severity := b.Add("severity", jev.Score("How severe?", "None", "Mild", "Serious"))
	resp, err := b.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "req_jevtest" || resp.Model != "jevtest" {
		t.Errorf("response = %+v", resp)
	}
	if s, _ := spam.Get(); s.P != 0.02 {
		t.Errorf("spam = %v", s)
	}
	if i, _ := intent.Get(); i.Value != Refund || i.Probs[Other] < 0.099 || i.Probs[Other] > 0.101 || i.Confidence != 0.9 {
		t.Errorf("intent = %+v", i)
	}
	sev, _ := severity.Get()
	if idx, level := sev.Likeliest(); idx != 2 || level != "Serious" || sev.Value < 1.6 || sev.Value > 1.8 {
		t.Errorf("severity = %+v", sev)
	}
	if calls := fake.Calls(); len(calls) != 1 || len(calls[0].Questions) != 3 || calls[0].Model != jev.DefaultModel {
		t.Errorf("calls = %+v", calls)
	}
}

func TestFakeFailsClosed(t *testing.T) {
	client, _ := jev.New(jev.WithProvider(jevtest.New()))
	_, err := client.Ask(t.Context(), "s", jev.Noul("unmatched"))
	if err == nil || !strings.Contains(err.Error(), "no rule matches") {
		t.Errorf("err = %v", err)
	}
	boom := errors.New("boom")
	client, _ = jev.New(jev.WithProvider(jevtest.New().On(jevtest.Any(), jevtest.Fail(boom))))
	if _, err := client.Ask(t.Context(), "s", jev.Noul("?")); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	client, _ = jev.New(jev.WithProvider(jevtest.New().On(jevtest.Not(jevtest.Kind(jev.KindNoul)), jevtest.Yes(1))))
	if _, err := client.Ask(t.Context(), "s", jev.Score("?", "a", "b")); err == nil || !strings.Contains(err.Error(), "Yes used on a score") {
		t.Errorf("err = %v", err)
	}
	if _, err := client.Ask(t.Context(), "s", jev.OneOf("?", Refund)); err == nil {
		t.Error("Not(noul) should not answer a choice with Yes")
	}
}
