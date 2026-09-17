package jev

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"time"
)

const (
	pathSystemOne = "/v1/systemone"
	pathModels    = "/v1/models"

	// Header names the official SDKs send and read. Go canonicalizes them;
	// the API matches case-insensitively.
	requestIDHeader    = "X-Typesafe-Request-Id"
	sdkHeader          = "X-Typesafe-Sdk"
	runtimeHeader      = "X-Typesafe-Runtime"
	retryCountHeader   = "X-Typesafe-Retry-Count"
	retryAfterHeader   = "Retry-After"
	retryAfterMSHeader = "Retry-After-Ms"

	// maxResponseBytes bounds memory per response; API bodies are kilobytes.
	maxResponseBytes = 16 << 20
)

// httpProvider is the default [Provider]: HTTP with retries, speaking the
// dialect of one API (TypeSafe directly, or Vercel AI Gateway).
type httpProvider struct {
	baseURL   string
	apiKey    string
	userAgent string
	headers   http.Header
	hc        *http.Client
	timeout   time.Duration
	retry     RetryPolicy
	logger    *slog.Logger
	wire      wire
}

// wire is one API's dialect: where to post, how to encode a [Request] and
// decode a [Response], and the protocol headers it expects. Retries,
// timeouts, logging, and error taxonomy are shared by the provider.
type wire interface {
	evaluatePath() string
	// requestIDHeader names the response header carrying the request id.
	requestIDHeader() string
	listsModels() bool
	encode(req *Request) ([]byte, error)
	// decode turns a 2xx body into a Response (without Meta) and any
	// warnings the API attached. Contract violations are a *ResponseError.
	decode(req *Request, res result) (*Response, []string, error)
	// setHeaders adds the dialect's protocol headers after the common ones.
	setHeaders(h http.Header, req *Request, attempt int)
}

// typesafeWire speaks the TypeSafe System One API.
type typesafeWire struct{}

func (typesafeWire) evaluatePath() string    { return pathSystemOne }
func (typesafeWire) requestIDHeader() string { return requestIDHeader }
func (typesafeWire) listsModels() bool       { return true }

func (typesafeWire) encode(req *Request) ([]byte, error) {
	return json.Marshal(req, json.Deterministic(true))
}

func (typesafeWire) decode(_ *Request, res result) (*Response, []string, error) {
	var out Response
	if err := json.Unmarshal(res.body, &out); err != nil {
		return nil, nil, newResponseError(res, jsonPath(err), err)
	}
	switch {
	case out.Model == "":
		return nil, nil, newResponseError(res, "model", errMissingField)
	case out.Answers == nil:
		return nil, nil, newResponseError(res, "answers", errMissingField)
	}
	return &out, nil, nil
}

func (typesafeWire) setHeaders(h http.Header, _ *Request, attempt int) {
	h.Set(sdkHeader, userAgent)
	h.Set(runtimeHeader, runtimeToken)
	h.Del(retryCountHeader)
	if attempt > 0 {
		h.Set(retryCountHeader, strconv.Itoa(attempt))
	}
}

// result is the outcome of the last HTTP attempt of a call.
type result struct {
	method   string
	url      string
	status   int
	header   http.Header
	body     []byte
	attempts int
	latency  time.Duration
	// reqIDHeader is the dialect's request id header, read by meta and errors.
	reqIDHeader string
}

func (r result) meta() Meta {
	return Meta{
		RequestID:  r.header.Get(r.reqIDHeader),
		StatusCode: r.status,
		Header:     r.header,
		Attempts:   r.attempts,
		Latency:    r.latency,
	}
}

// Evaluate implements [Provider].
func (p *httpProvider) Evaluate(ctx context.Context, req *Request) (*Response, error) {
	body, err := p.wire.encode(req)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding request: %w", ErrInvalidRequest, err)
	}
	res, err := p.do(ctx, http.MethodPost, p.wire.evaluatePath(), body, func(h http.Header, attempt int) {
		p.wire.setHeaders(h, req, attempt)
	})
	if err != nil {
		return nil, err
	}
	out, warnings, err := p.wire.decode(req, res)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		p.logger.LogAttrs(ctx, slog.LevelWarn, "jev: api warning",
			slog.String("endpoint", res.method+" "+res.url), slog.String("warning", w))
	}
	out.Meta = res.meta()
	return out, nil
}

// Models implements [ModelLister]. Only the TypeSafe API lists models;
// through Vercel AI Gateway it fails with [ErrNoModelList].
func (p *httpProvider) Models(ctx context.Context) ([]Model, error) {
	if !p.wire.listsModels() {
		return nil, ErrNoModelList
	}
	res, err := p.do(ctx, http.MethodGet, pathModels, nil, func(h http.Header, attempt int) {
		p.wire.setHeaders(h, nil, attempt)
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(res.body, &out); err != nil {
		return nil, newResponseError(res, jsonPath(err), err)
	}
	if out.Models == nil {
		return nil, newResponseError(res, "models", errMissingField)
	}
	return out.Models, nil
}

var errMissingField = errors.New("missing required field")

func jsonPath(err error) string {
	var semantic *json.SemanticError
	if errors.As(err, &semantic) && semantic.JSONPointer != "" {
		return string(semantic.JSONPointer)
	}
	return ""
}

// do sends one API call with retries. extra adds the dialect's headers to
// each attempt. The result is returned even on error so callers can read
// the attempt count.
func (p *httpProvider) do(ctx context.Context, method, path string, body []byte, extra func(http.Header, int)) (res result, err error) {
	res = result{method: method, url: p.baseURL + path, reqIDHeader: p.wire.requestIDHeader()}
	callCtx := ctx
	if p.retry.TotalTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, p.retry.TotalTimeout)
		defer cancel()
	}
	started := time.Now()
	defer func() { res.latency = time.Since(started) }()

	for attempt := 0; ; attempt++ {
		err = p.attempt(ctx, callCtx, &res, body, attempt, extra)
		if err == nil {
			return res, nil
		}
		switch {
		case ctx.Err() != nil:
			return res, p.canceled(res, ctx.Err())
		case callCtx.Err() != nil:
			return res, p.budgetExceeded(res, started, callCtx.Err())
		case attempt >= p.retry.MaxRetries || !p.retry.retryable(err):
			return res, err
		}
		delay := p.retry.delay(attempt, err)
		if p.retry.TotalTimeout > 0 && time.Since(started)+delay >= p.retry.TotalTimeout {
			p.logger.LogAttrs(ctx, slog.LevelInfo, "jev: retry budget exhausted",
				slog.String("endpoint", method+" "+res.url), slog.Duration("total_timeout", p.retry.TotalTimeout),
				slog.Int("attempts", res.attempts))
			return res, err
		}
		p.logger.LogAttrs(ctx, slog.LevelInfo, "jev: retrying",
			slog.String("endpoint", method+" "+res.url), slog.Duration("delay", delay),
			slog.Int("retry", attempt+1), slog.Int("max_retries", p.retry.MaxRetries),
			slog.String("cause", err.Error()))
		if err := sleep(callCtx, delay); err != nil {
			if ctx.Err() != nil {
				return res, p.canceled(res, ctx.Err())
			}
			return res, p.budgetExceeded(res, started, err)
		}
	}
}

func (p *httpProvider) canceled(res result, err error) error {
	return fmt.Errorf("jev: %s %s: %w", res.method, res.url, err)
}

func (p *httpProvider) budgetExceeded(res result, started time.Time, err error) error {
	return &ConnectionError{
		Endpoint: res.method + " " + res.url, Elapsed: time.Since(started), Timeout: p.retry.TotalTimeout,
		StatusCode: res.status, Header: res.header, RequestID: res.header.Get(res.reqIDHeader),
		Attempts: res.attempts, Err: err,
	}
}

// attempt performs one HTTP round trip. ctx is the caller's context and
// callCtx additionally carries the TotalTimeout deadline.
func (p *httpProvider) attempt(ctx, callCtx context.Context, res *result, body []byte, attempt int, extra func(http.Header, int)) error {
	res.status, res.header, res.body = 0, nil, nil
	attemptCtx := callCtx
	if p.timeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(callCtx, p.timeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(attemptCtx, res.method, res.url, reader)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	p.setHeaders(req, body != nil)
	if extra != nil {
		extra(req.Header, attempt)
	}
	if p.logger.Enabled(ctx, slog.LevelDebug) {
		p.logger.LogAttrs(ctx, slog.LevelDebug, "jev: request",
			slog.String("endpoint", res.method+" "+res.url), slog.Int("attempt", attempt+1),
			slog.Any("headers", redactedHeaders(req.Header)), slog.String("body", string(body)))
	}

	res.attempts++
	started := time.Now()
	httpRes, err := p.hc.Do(req)
	if err == nil {
		res.status = httpRes.StatusCode
		res.header = httpRes.Header
		res.body, err = readBody(httpRes.Body)
	}
	elapsed := time.Since(started)
	if errors.Is(err, ErrResponseTooLarge) {
		return newResponseError(*res, "", err)
	}
	if err != nil {
		if ctx.Err() != nil {
			return p.canceled(*res, ctx.Err())
		}
		connErr := &ConnectionError{
			Endpoint: res.method + " " + res.url, Elapsed: elapsed, StatusCode: res.status, Header: res.header,
			RequestID: res.header.Get(res.reqIDHeader), Attempts: res.attempts, Err: err,
		}
		switch {
		case p.retry.TotalTimeout > 0 && callCtx.Err() != nil:
			connErr.Timeout = p.retry.TotalTimeout
		case errors.Is(attemptCtx.Err(), context.DeadlineExceeded):
			connErr.Timeout = p.timeout
		}
		p.logger.LogAttrs(ctx, slog.LevelInfo, "jev: request failed",
			slog.String("endpoint", res.method+" "+res.url), slog.Duration("elapsed", elapsed),
			slog.String("error", connErr.Error()))
		return connErr
	}

	p.logger.LogAttrs(ctx, slog.LevelInfo, "jev: response",
		slog.String("endpoint", res.method+" "+res.url), slog.Int("status", res.status),
		slog.Duration("elapsed", elapsed), slog.String("request_id", res.header.Get(res.reqIDHeader)),
		slog.Int("attempt", attempt+1))
	if p.logger.Enabled(ctx, slog.LevelDebug) {
		p.logger.LogAttrs(ctx, slog.LevelDebug, "jev: response body",
			slog.Any("headers", redactedHeaders(res.header)), slog.String("body", string(res.body)))
	}
	if res.status >= 200 && res.status < 300 {
		return nil
	}
	return newAPIError(*res)
}

func readBody(body io.ReadCloser) ([]byte, error) {
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

// setHeaders applies caller headers first so the protected ones always win.
// The dialect's own headers are added afterwards by the wire.
func (p *httpProvider) setHeaders(req *http.Request, hasBody bool) {
	maps.Copy(req.Header, p.headers)
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.userAgent)
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Del("Content-Type")
	}
}
