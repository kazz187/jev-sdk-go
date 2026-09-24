package jev_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kazz187/jev-sdk-go"
)

func TestAPIKeyResolution(t *testing.T) {
	t.Setenv(jev.EnvAPIKey, "")
	if _, err := jev.New(); !errors.Is(err, jev.ErrNoAPIKey) {
		t.Errorf("no key anywhere: %v", err)
	}
	t.Setenv(jev.EnvAPIKey, " env-key ")
	if _, err := jev.New(); err != nil {
		t.Errorf("key from env: %v", err)
	}
	if _, err := jev.New(jev.WithAPIKey("")); !errors.Is(err, jev.ErrNoAPIKey) {
		t.Errorf("explicit empty key must not fall back to the environment: %v", err)
	}
	if _, err := jev.New(jev.WithProvider(jev.ProviderFunc(nil))); err != nil {
		t.Errorf("a provider needs no key: %v", err)
	}
	t.Setenv(jev.EnvAPIKey, "")
	if _, err := jev.New(jev.WithoutAPIKey()); err != nil {
		t.Errorf("WithoutAPIKey: %v", err)
	}
	if _, err := jev.New(jev.WithoutAPIKey(), jev.WithAPIKey("")); !errors.Is(err, jev.ErrNoAPIKey) {
		t.Errorf("a later WithAPIKey must replace WithoutAPIKey: %v", err)
	}
	if _, err := jev.New(jev.WithoutAPIKey(), jev.WithVercelAIGateway()); !errors.Is(err, jev.ErrInvalidConfig) {
		t.Errorf("WithoutAPIKey with the gateway: %v", err)
	}
}

func TestEnvironmentFallbacks(t *testing.T) {
	t.Setenv(jev.EnvAPIKey, "k")
	t.Setenv(jev.EnvDefaultModel, "jev-env")
	t.Setenv(jev.EnvBaseURL, "http://env.example/")
	client, err := jev.New()
	if err != nil {
		t.Fatal(err)
	}
	if client.DefaultModel() != "jev-env" {
		t.Errorf("model = %q", client.DefaultModel())
	}
	client, err = jev.New(jev.WithModel("jev-explicit"))
	if err != nil || client.DefaultModel() != "jev-explicit" {
		t.Errorf("explicit model: %q, %v", client.DefaultModel(), err)
	}
	t.Setenv(jev.EnvDefaultModel, "")
	client, _ = jev.New()
	if client.DefaultModel() != jev.DefaultModel {
		t.Errorf("default model = %q", client.DefaultModel())
	}
}

func TestInvalidConfiguration(t *testing.T) {
	bad := jev.DefaultRetryPolicy()
	bad.Jitter = 2
	cases := map[string][]jev.Option{
		"zero timeout":     {jev.WithTimeout(0)},
		"negative retries": {jev.WithMaxRetries(-1)},
		"jitter":           {jev.WithRetryPolicy(bad)},
		"url scheme":       {jev.WithBaseURL("ftp://x")},
		"url credentials":  {jev.WithBaseURL("https://user:pw@x")},
		"url query":        {jev.WithBaseURL("https://x/?a=1")},
		"url no host":      {jev.WithBaseURL("https://")},
		"empty user agent": {jev.WithUserAgent(" ")},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := jev.New(append([]jev.Option{jev.WithAPIKey("k")}, opts...)...)
			if !errors.Is(err, jev.ErrInvalidConfig) {
				t.Errorf("err = %v", err)
			}
			if strings.Contains(err.Error(), "user:pw") {
				t.Errorf("error echoes credentials: %v", err)
			}
		})
	}
	t.Setenv(jev.EnvLogLevel, "loud")
	if _, err := jev.New(jev.WithAPIKey("k")); !errors.Is(err, jev.ErrInvalidConfig) {
		t.Errorf("bad log level: %v", err)
	}
}

func TestLoggingRedactsCredentials(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okBody)
	}))
	client, err := jev.New(
		jev.WithAPIKey("sk-live-1234567890abcdef"),
		jev.WithBaseURL("http://jev.test"),
		jev.WithHTTPClient(srv.Client()),
		jev.WithLogger(logger),
		jev.WithHeader("X-Session-Token", "tok"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Ask(t.Context(), "s", jev.Noul("?")); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if strings.Contains(logs, "1234567890abcdef") || strings.Contains(logs, "tok\"") {
		t.Errorf("logs leak secrets:\n%s", logs)
	}
	for _, want := range []string{"Bearer ***cdef", "X-Session-Token=[***]", "jev: response", `instructions`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs miss %q:\n%s", want, logs)
		}
	}
}

func TestLogLevelFromEnvironment(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	t.Setenv(jev.EnvLogLevel, "info")

	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okBody)
	}))
	client, err := jev.New(jev.WithAPIKey("k"), jev.WithBaseURL("http://jev.test"), jev.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Ask(t.Context(), "s", jev.Noul("?")); err != nil {
		t.Fatal(err)
	}
	if logs := buf.String(); !strings.Contains(logs, "jev: response") || strings.Contains(logs, "jev: request body") {
		t.Errorf("info level logs:\n%s", logs)
	}
}

func TestHTTPClientIsNotMutated(t *testing.T) {
	hc := &http.Client{Timeout: 42 * time.Second}
	if _, err := jev.New(jev.WithAPIKey("k"), jev.WithHTTPClient(hc), jev.WithTimeout(time.Second)); err != nil {
		t.Fatal(err)
	}
	if hc.Timeout != 42*time.Second {
		t.Errorf("Timeout was changed to %v", hc.Timeout)
	}
}

func TestAPIErrorTaxonomy(t *testing.T) {
	cases := []struct {
		status int
		is     []error
		isNot  []error
	}{
		{400, []error{jev.ErrBadRequest}, []error{jev.ErrUnprocessable, jev.ErrInternalServer}},
		{401, []error{jev.ErrAuthentication}, []error{jev.ErrPermissionDenied}},
		{403, []error{jev.ErrPermissionDenied}, []error{jev.ErrAuthentication}},
		{404, []error{jev.ErrNotFound}, nil},
		{422, []error{jev.ErrUnprocessable}, []error{jev.ErrBadRequest}},
		{429, []error{jev.ErrRateLimit}, []error{jev.ErrInternalServer}},
		{500, []error{jev.ErrInternalServer}, []error{jev.ErrOverloaded}},
		{529, []error{jev.ErrOverloaded, jev.ErrInternalServer}, nil},
	}
	for _, tc := range cases {
		err := &jev.APIError{StatusCode: tc.status}
		for _, target := range tc.is {
			if !errors.Is(err, target) {
				t.Errorf("%d should match %v", tc.status, target)
			}
		}
		for _, target := range tc.isNot {
			if errors.Is(err, target) {
				t.Errorf("%d should not match %v", tc.status, target)
			}
		}
	}
}

func TestAPIErrorMessages(t *testing.T) {
	cases := map[string]string{
		`"just a string"`:   "just a string",
		`{"error":"plain"}`: "plain",
		`{"error":{"error_type":"auth_error","message":"bad key"}}`: "auth_error: bad key",
		`{"message":"top level"}`:                                   "top level",
		`{"detail":"detail string"}`:                                "detail string",
		`{"detail":{"message":"detail object"}}`:                    "detail object",
		`{"detail":[{"loc":["body","questions","q"],"msg":"field required","type":"missing"},{"msg":"other"}]}`: "questions.q: field required; other",
		`<html>`:             "<html>",
		``:                   "(no body)",
		`{"unrelated":true}`: `{"unrelated":true}`,
	}
	for body, want := range cases {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, body)
		}))
		client, _ := jev.New(jev.WithAPIKey("k"), jev.WithBaseURL("http://jev.test"), jev.WithHTTPClient(srv.Client()))
		_, err := client.Ask(context.Background(), "s", jev.Noul("?"))
		if !strings.Contains(err.Error(), "400 Bad Request "+want) {
			t.Errorf("body %s: Error() = %q, want to contain %q", body, err, want)
		}
	}
	long := &jev.APIError{StatusCode: 500, Message: strings.Repeat("あ", 300)}
	if msg := long.Error(); strings.Count(msg, "あ") != 200 || !strings.HasSuffix(msg, "…") {
		t.Errorf("truncation: %q", msg)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Now()
	cases := map[string]struct {
		header http.Header
		want   time.Duration
	}{
		"ms":        {http.Header{"Retry-After-Ms": {"250"}}, 250 * time.Millisecond},
		"ms wins":   {http.Header{"Retry-After-Ms": {"250"}, "Retry-After": {"5"}}, 250 * time.Millisecond},
		"seconds":   {http.Header{"Retry-After": {"2.5"}}, 2500 * time.Millisecond},
		"date":      {http.Header{"Retry-After": {now.Add(10 * time.Second).UTC().Format(http.TimeFormat)}}, 10 * time.Second},
		"past date": {http.Header{"Retry-After": {now.Add(-time.Hour).UTC().Format(http.TimeFormat)}}, 0},
		"garbage":   {http.Header{"Retry-After": {"soon"}}, 0},
		"negative":  {http.Header{"Retry-After": {"-1"}}, 0},
		"none":      {http.Header{}, 0},
	}
	for name, tc := range cases {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range tc.header {
				w.Header()[k] = v
			}
			w.WriteHeader(http.StatusBadRequest)
		}))
		client, _ := jev.New(jev.WithAPIKey("k"), jev.WithBaseURL("http://jev.test"), jev.WithHTTPClient(srv.Client()))
		_, err := client.Ask(context.Background(), "s", jev.Noul("?"))
		var apiErr *jev.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("%s: %v", name, err)
		}
		// Dates are relative to the parse time; allow a second of slack.
		if diff := apiErr.RetryAfter - tc.want; diff < -time.Second || diff > time.Second {
			t.Errorf("%s: RetryAfter = %v, want %v", name, apiErr.RetryAfter, tc.want)
		}
	}
}
