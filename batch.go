package jev

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"
)

// Batch collects questions about one state and evaluates them in a single
// request. Build it with [Client.Batch], add questions with [Batch.Add],
// then call [Batch.Run] once. A Batch is not safe for concurrent use.
type Batch struct {
	client   *Client
	state    any
	model    string
	extra    map[string]any
	specs    map[string]Spec
	decoders []func(map[string]RawAnswer) error
	buildErr error // invalid questions, reported by Run
	evalErr  error // provider failure, reported by every handle
	ran      bool
}

// Batch opens a batch of questions about one state. The state may be plain
// text or any value that encodes as a JSON object or array; give its parts
// names and a question can point at the one it is about.
func (c *Client) Batch(state any) *Batch {
	return &Batch{client: c, state: state, specs: make(map[string]Spec)}
}

// Model overrides the client's default model for this batch.
func (b *Batch) Model(model string) *Batch {
	b.model = model
	return b
}

// Extra adds top-level request fields this package does not model. It is
// the counterpart of the official SDKs' extra_body.
func (b *Batch) Extra(fields map[string]any) *Batch {
	if b.extra == nil {
		b.extra = make(map[string]any, len(fields))
	}
	maps.Copy(b.extra, fields)
	return b
}

// Len reports how many questions have been added.
func (b *Batch) Len() int { return len(b.decoders) }

// Handle is a typed slot for one answer in a [Batch].
type Handle[A any] struct {
	name  string
	batch *Batch
	value A
	err   error
}

// Name returns the name the question was added under.
func (h *Handle[A]) Name() string { return h.name }

// Add appends q to the batch under name and returns a handle for its
// answer. The answer comes back under the same name in [Response.Answers].
// An invalid question does not panic: its error is returned by [Batch.Run]
// and by [Handle.Get].
func (b *Batch) Add[A any](name string, q Question[A]) *Handle[A] {
	h := &Handle[A]{name: name, batch: b}
	fail := func(err error) *Handle[A] {
		h.err = err
		b.buildErr = errors.Join(b.buildErr, err)
		b.decoders = append(b.decoders, func(map[string]RawAnswer) error { return nil })
		return h
	}
	switch {
	case b.ran:
		h.err = ErrBatchUsed
		return h
	case name == "":
		return fail(invalid("question name must not be empty"))
	case q == nil:
		return fail(invalid("question %q is nil", name))
	}
	if _, dup := b.specs[name]; dup {
		return fail(invalid("question %q was added twice", name))
	}
	spec, err := q.spec()
	if err != nil {
		return fail(fmt.Errorf("question %q: %w", name, err))
	}
	b.specs[name] = spec
	b.decoders = append(b.decoders, func(answers map[string]RawAnswer) error {
		raw, ok := answers[name]
		if !ok {
			h.err = malformed("no answer for question %q", name)
			return h.err
		}
		v, err := q.decode(raw)
		if err != nil {
			h.err = fmt.Errorf("question %q: %w", name, err)
			return h.err
		}
		h.value = v
		return nil
	})
	return h
}

// Run sends the batch and returns the response. It returns an error when a
// question was invalid, the provider failed, or an answer did not fit its
// question; in the last case the response is still returned and the handles
// whose answers were fine can still be read.
func (b *Batch) Run(ctx context.Context) (*Response, error) {
	if b.ran {
		return nil, ErrBatchUsed
	}
	b.ran = true
	if b.buildErr != nil {
		b.evalErr = b.buildErr
		return nil, b.buildErr
	}
	if len(b.specs) == 0 {
		b.evalErr = invalid("batch has no questions")
		return nil, b.evalErr
	}
	for _, key := range []string{"model", "state", "questions"} {
		if _, clash := b.extra[key]; clash {
			b.evalErr = invalid("extra field %q is reserved", key)
			return nil, b.evalErr
		}
	}
	model := b.model
	if model == "" {
		model = b.client.model
	}
	req := &Request{Model: model, State: b.state, Questions: b.specs, Extra: b.extra}

	start := time.Now()
	resp, err := b.client.provider.Evaluate(ctx, req)
	if err == nil && resp == nil {
		err = malformed("provider returned no response")
	}
	if err != nil {
		b.evalErr = err
		return nil, err
	}
	if resp.Latency == 0 {
		resp.Latency = time.Since(start)
	}

	var errs []error
	for _, decode := range b.decoders {
		if err := decode(resp.Answers); err != nil {
			errs = append(errs, err)
		}
	}
	return resp, errors.Join(errs...)
}

// Get returns the typed answer. It returns [ErrNotRun] before the batch ran,
// the batch's error when the request failed, and a [ErrMalformedAnswer]
// error when this answer did not fit its question.
func (h *Handle[A]) Get() (A, error) {
	var zero A
	switch {
	case h.err != nil:
		return zero, h.err
	case !h.batch.ran:
		return zero, ErrNotRun
	case h.batch.evalErr != nil:
		return zero, h.batch.evalErr
	}
	return h.value, nil
}

// Ask evaluates a single question about state and returns its typed answer.
// For several independent questions about the same state, use a [Batch]:
// it costs one request instead of one per question.
func (c *Client) Ask[A any](ctx context.Context, state any, q Question[A]) (A, error) {
	b := c.Batch(state)
	h := b.Add("q", q)
	if _, err := b.Run(ctx); err != nil {
		var zero A
		return zero, err
	}
	return h.Get()
}
