package jev_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/kazz187/jev-sdk-go"
	"github.com/kazz187/jev-sdk-go/jevtest"
)

type Department string

const (
	Sales   Department = "sales"
	Support Department = "support"
	Unknown Department = "unknown"
)

// route is declared once and reused across requests.
var route = jev.Choice("Which department should read this email?",
	jev.Opt(Sales, "pricing, quotes, new purchases"),
	jev.Opt(Support, "problems with an existing purchase"),
	jev.Opt(Unknown, nil),
)

func ExampleClient_Ask() {
	fake := jevtest.New().On(jevtest.Option("sales"), jevtest.Pick("sales", 0.95))
	client, err := jev.New(jev.WithProvider(fake)) // jev.New() alone reads TYPESAFE_API_KEY
	if err != nil {
		log.Fatal(err)
	}

	answer, err := client.Ask(context.Background(), "How much is the team plan?", route)
	if err != nil {
		log.Fatal(err)
	}
	department, sure := answer.Sure(0.9) // department is a Department
	fmt.Println(department, sure)
	// Output: sales true
}

func ExampleBatch() {
	fake := jevtest.New().
		On(jevtest.Kind(jev.KindNoul), jevtest.Yes(0.8)).
		On(jevtest.Kind(jev.KindScore), jevtest.Level(1, 0.7))
	client, _ := jev.New(jev.WithProvider(fake))

	b := client.Batch(map[string]string{
		"subject": "Server down since 9am",
		"body":    "Nothing works, please call me back today.",
	})
	urgent := b.Add("urgent", jev.Noul("Does the email ask for action today?"))
	anger := b.Add("anger", jev.Score("How frustrated is the sender?", "Calm", "Frustrated", "Furious"))
	if _, err := b.Run(context.Background()); err != nil {
		log.Fatal(err)
	}

	u, _ := urgent.Get()
	a, _ := anger.Get()
	_, level := a.Nearest()
	fmt.Printf("urgent=%.1f anger=%v\n", u.P, level)
	// Output: urgent=0.8 anger=Frustrated
}

func ExampleAPIError() {
	client, _ := jev.New(jev.WithProvider(jev.ProviderFunc(func(context.Context, *jev.Request) (*jev.Response, error) {
		return nil, &jev.APIError{StatusCode: 429, Endpoint: "POST /v1/systemone", Message: "slow down"}
	})))
	_, err := client.Ask(context.Background(), "hello", jev.Noul("Is this a greeting?"))

	var apiErr *jev.APIError
	fmt.Println(errors.Is(err, jev.ErrRateLimit), errors.As(err, &apiErr), apiErr.StatusCode)
	// Output: true true 429
}
