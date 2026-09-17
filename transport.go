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

// httpProvider is the default [Provider], talking to the TypeSafe HTTP API
// with retries.
type httpProvider struct {
	baseURL   string
	apiKey    string
	userAgent string
	headers   http.Header
	hc        *http.Client
	timeout   time.Duration
	retry     RetryPolicy
	logger    *slog.Logger
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
}

func (r result) meta() Meta {
	return Meta{
		RequestID:  r.header.Get(requestIDHeader),
		StatusCode: r.status,
		Header:     r.header,
		Attempts:   r.attempts,
		Latency:    r.latency,
	}
}

// Evaluate implements [Provider].
func (p *httpProvider) Evaluate(ctx context.Context, req *Request) (*Response, error) {
	body, err := json.Marshal(req, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("%w: encoding request: %w", ErrInvalidRequest, err)
	}
	res, err := p.do(ctx, http.MethodPost, pathSystemOne, body)
	if err != nil {
		return nil, err
	}
	var out Response
	if err := json.Unmarshal(res.body, &out); err != nil {
		return nil, newResponseError(res, jsonPath(err), err)
	}
	switch {
	case out.Model == "":
		return nil, newResponseError(res, "model", errMissingField)
	case out.Answers == nil:
		return nil, newResponseError(res, "answers", errMissingField)
	}
	out.Meta = res.meta()
	return &out, nil
}

// Models implements [ModelLister].
func (p *httpProvider) Models(ctx context.Context) ([]Model, error) {
	res, err := p.do(ctx, http.MethodGet, pathModels, nil)
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

// do sends one API call with retries. The result is returned even on error
// so callers can read the attempt count.
func (p *httpProvider) do(ctx context.Context, method, path string, body []byte) (res result, err error) {
	res = result{method: method, url: p.baseURL + path}
	callCtx := ctx
	if p.retry.TotalTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, p.retry.TotalTimeout)
		defer cancel()
	}
	started := time.Now()
	defer func() { res.latency = time.Since(started) }()

	for attempt := 0; ; attempt++ {
		err = p.attempt(ctx, callCtx, &res, body, attempt)
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
		StatusCode: res.status, Header: res.header, RequestID: res.header.Get(requestIDHeader),
		Attempts: res.attempts, Err: err,
	}
}

// attempt performs one HTTP round trip. ctx is the caller's context and
// callCtx additionally carries the TotalTimeout deadline.
func (p *httpProvider) attempt(ctx, callCtx context.Context, res *result, body []byte, attempt int) error {
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
	p.setHeaders(req, body != nil, attempt)
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
			RequestID: res.header.Get(requestIDHeader), Attempts: res.attempts, Err: err,
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
		slog.Duration("elapsed", elapsed), slog.String("request_id", res.header.Get(requestIDHeader)),
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
func (p *httpProvider) setHeaders(req *http.Request, hasBody bool, attempt int) {
	maps.Copy(req.Header, p.headers)
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set(sdkHeader, userAgent)
	req.Header.Set(runtimeHeader, runtimeToken)
	req.Header.Del(retryCountHeader)
	if attempt > 0 {
		req.Header.Set(retryCountHeader, strconv.Itoa(attempt))
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Del("Content-Type")
	}
}
