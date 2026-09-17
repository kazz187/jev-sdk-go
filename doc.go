// Package jev is a Go client for the TypeSafe AI System One API and its
// model, Jev. It requires Go 1.27 and uses generic methods so that every
// call site reads left to right and returns the caller's own types.
//
// A request sends one state and one or more questions about it. Each
// question is one of three kinds: [Noul] (is this true?), [Choice] (which
// one of these?), and [Score] (where on this scale?). Answers are typed by
// the question that produced them, so a [Choice] over a string enum returns
// that enum and the compiler checks the switch written on it.
//
// # One question
//
//	type Team string
//
//	const (
//		Billing   Team = "billing"
//		Technical Team = "technical"
//		Other     Team = "other"
//	)
//
//	client, err := jev.New() // reads TYPESAFE_API_KEY
//	...
//	team, err := client.Ask(ctx, ticket, jev.Choice("Which team should handle this?",
//		jev.Opt(Billing, "charges, invoices, refunds"),
//		jev.Opt(Technical, "bugs, outages, integrations"),
//		jev.Opt(Other, nil),
//	))
//	if err != nil {
//		return err
//	}
//	if value, sure := team.Sure(0.9); sure {
//		route(value) // value is a Team
//	}
//
// # Many questions, one request
//
// Independent questions about the same state belong in one [Batch]: they are
// answered together for the price of one round trip. [Batch.Add] is a
// generic method, so each handle carries the answer type of its question:
//
//	b := client.Batch(review)
//	spam := b.Add("spam", jev.Noul("Is this review spam or advertising?"))
//	tone := b.Add("tone", jev.Score("How angry is the reviewer?", "Calm", "Annoyed", "Furious"))
//	resp, err := b.Run(ctx)
//	...
//	s, err := spam.Get() // jev.NoulAnswer
//	t, err := tone.Get() // jev.ScoreAnswer
//
// # Policy stays in your code
//
// Low confidence is not an error. Answers carry the full distribution, and
// the thresholds that decide what to do with it belong to the caller.
// Errors are reserved for invalid questions, transport and API failures, and
// answers that do not fit the question.
//
// # Configuration
//
// [New] reads TYPESAFE_API_KEY, TYPESAFE_BASE_URL, TYPESAFE_DEFAULT_MODEL,
// and TYPESAFE_LOG_LEVEL when the matching option is not given. Explicit
// options win over the environment, which wins over the defaults. Defaults
// for timeouts, retries, and headers match the official TypeSafe SDKs.
//
// # Testing
//
// Package [github.com/kazz187/jev-sdk-go/jevtest] provides a rule-based
// fake [Provider], so code that branches on Jev can be tested without the
// network.
package jev
