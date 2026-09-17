package jev

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"
)

// RetryPolicy controls how failed attempts are retried. Start from
// [DefaultRetryPolicy] and change what you need, or use [WithMaxRetries]. A
// zero RetryPolicy disables retries.
type RetryPolicy struct {
	// MaxRetries is the number of retries after the initial attempt.
	// Default: 2.
	MaxRetries int
	// InitialBackoff is the first backoff delay, doubled each retry up to
	// MaxBackoff. Default: 500ms.
	InitialBackoff time.Duration
	// MaxBackoff caps a single backoff delay. Default: 5s.
	MaxBackoff time.Duration
	// Jitter is the fraction of each backoff delay randomly subtracted, from
	// 0 to 1. Default: 0.25.
	Jitter float64
	// Statuses lists the HTTP status codes that are retried. Nil means the
	// default: 408, 429, and every 5xx.
	Statuses []int
	// RespectRetryAfter honors retry-after-ms and Retry-After response
	// headers up to MaxRetryAfter; longer delays fall back to backoff.
	// Default: true.
	RespectRetryAfter bool
	// MaxRetryAfter is the longest server-requested delay honored.
	// Default: 60s.
	MaxRetryAfter time.Duration
	// ConnectionErrors retries failures that produced no HTTP response.
	// Default: true.
	ConnectionErrors bool
	// Timeouts retries attempts that ran out of time. Default: true.
	Timeouts bool
	// TotalTimeout bounds the whole call, attempts and delays included. When
	// it expires the call fails with a [*ConnectionError] matching
	// [ErrTimeout]; a retry whose delay would reach it is skipped and the
	// last error returned. Zero disables it; the caller's context deadline
	// always applies. Default: 30s, as in the official Python SDK.
	TotalTimeout time.Duration
	// ShouldRetry, when set, is consulted for API and connection errors the
	// rules above do not retry. It never sees context errors or response
	// validation errors, which are never retried.
	ShouldRetry func(error) bool
}

// DefaultRetryPolicy returns the policy used when none is supplied. It
// matches the official TypeSafe SDKs.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries:        2,
		InitialBackoff:    500 * time.Millisecond,
		MaxBackoff:        5 * time.Second,
		Jitter:            0.25,
		RespectRetryAfter: true,
		MaxRetryAfter:     time.Minute,
		ConnectionErrors:  true,
		Timeouts:          true,
		TotalTimeout:      30 * time.Second,
	}
}

func (p RetryPolicy) validate() error {
	switch {
	case p.MaxRetries < 0:
		return fmt.Errorf("%w: MaxRetries must not be negative, got %d", ErrInvalidConfig, p.MaxRetries)
	case p.InitialBackoff < 0 || p.MaxBackoff < 0 || p.MaxRetryAfter < 0 || p.TotalTimeout < 0:
		return fmt.Errorf("%w: retry delays must not be negative", ErrInvalidConfig)
	case math.IsNaN(p.Jitter) || p.Jitter < 0 || p.Jitter > 1:
		return fmt.Errorf("%w: Jitter must be between 0 and 1, got %v", ErrInvalidConfig, p.Jitter)
	}
	for _, status := range p.Statuses {
		if status < 100 || status > 999 {
			return fmt.Errorf("%w: Statuses must be HTTP status codes, got %d", ErrInvalidConfig, status)
		}
	}
	return nil
}

func (p RetryPolicy) retryableStatus(status int) bool {
	if p.Statuses == nil {
		return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
	}
	return slices.Contains(p.Statuses, status)
}

func (p RetryPolicy) retryable(err error) bool {
	var conn *ConnectionError
	var api *APIError
	switch {
	case errors.As(err, &conn):
		if errors.Is(err, ErrTimeout) {
			if p.Timeouts {
				return true
			}
		} else if p.ConnectionErrors {
			return true
		}
	case errors.As(err, &api):
		if p.retryableStatus(api.StatusCode) {
			return true
		}
	default:
		return false
	}
	return p.ShouldRetry != nil && p.ShouldRetry(err)
}

// delay is the wait before retry number attempt (zero-based): the server's
// Retry-After when allowed, otherwise capped exponential backoff with jitter.
func (p RetryPolicy) delay(attempt int, err error) time.Duration {
	var api *APIError
	if p.RespectRetryAfter && errors.As(err, &api) && api.RetryAfter > 0 && api.RetryAfter <= p.MaxRetryAfter {
		return api.RetryAfter
	}
	if p.InitialBackoff <= 0 || p.MaxBackoff <= 0 {
		return 0
	}
	backoff := math.Min(float64(p.InitialBackoff)*math.Pow(2, float64(attempt)), float64(p.MaxBackoff))
	return time.Duration(backoff * (1 - rand.Float64()*p.Jitter))
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
