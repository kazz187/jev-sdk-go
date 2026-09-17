package jev

import (
	"context"
	"net/http"
	"time"
)

// Request is one call to the System One API: a model, a state, and the
// questions to ask about it, each under a name of the caller's choosing.
// [Batch.Run] builds it; providers and fakes receive it.
type Request struct {
	Model     string          `json:"model"`
	State     any             `json:"state"`
	Questions map[string]Spec `json:"questions"`
	// Extra carries top-level request fields this package does not model,
	// the counterpart of the official SDKs' extra_body. "model", "state",
	// and "questions" are reserved.
	Extra map[string]any `json:",embed"`
}

// Usage counts the tokens a request consumed. The API may omit either
// counter, in which case it is zero.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Meta carries the transport details of a successful call. It is embedded
// in [Response], so its fields read as the response's own.
type Meta struct {
	// RequestID is the x-typesafe-request-id header, which TypeSafe support
	// will ask for. It is empty when the provider reports none.
	RequestID  string
	StatusCode int
	Header     http.Header
	// Attempts is the number of HTTP attempts made, including retries.
	Attempts int
	// Latency is the wall time of the whole call, retries included.
	Latency time.Duration
}

// Response is what came back for a [Request]: the model that answered, one
// raw answer per question, token usage, and the transport [Meta].
type Response struct {
	Meta    `json:"-"`
	Model   string               `json:"model"`
	Answers map[string]RawAnswer `json:"answers"`
	Usage   Usage                `json:"usage"`
}

// Model describes one model the account may use, from [Client.Models].
type Model struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// ReleaseDate is kept as sent. The API documents YYYY-MM-DD.
	ReleaseDate string `json:"release_date"`
}

// Provider evaluates requests. The default provider talks to the TypeSafe
// HTTP API; tests use [github.com/kazz187/jev-sdk-go/jevtest].
// Implementations must be safe for concurrent use.
type Provider interface {
	Evaluate(ctx context.Context, req *Request) (*Response, error)
}

// ModelLister is the optional half of [Provider], implemented by providers
// that can also list models. [Client.Models] needs it.
type ModelLister interface {
	Models(ctx context.Context) ([]Model, error)
}

// ProviderFunc adapts a function to [Provider].
type ProviderFunc func(ctx context.Context, req *Request) (*Response, error)

// Evaluate implements [Provider].
func (f ProviderFunc) Evaluate(ctx context.Context, req *Request) (*Response, error) {
	return f(ctx, req)
}

// Middleware wraps a [Provider], for caching, metrics, rate limiting, or
// tracing. See [WithMiddleware].
type Middleware func(next Provider) Provider
