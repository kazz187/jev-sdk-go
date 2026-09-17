package jev_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kazz187/jev-sdk-go"
)

const gatewayOKBody = `{
  "answers": {
    "spam":  {"type": "boolean", "probability": 0.02},
    "route": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.9, "shipping": 0.1}},
    "tone":  {"type": "score", "score": 1.5, "probabilities": {"0": 0, "1": 0.5, "2": 0.5}}
  },
  "usage": {"inputTokens": 120, "outputTokens": 3},
  "rounding": {"probabilityDecimals": 2},
  "warnings": [{"type": "other", "message": "beta"}],
  "providerMetadata": {"typesafe": {"confidence": {"route": 0.8, "tone": 0.5}}}
}`

type gwRoute string

const (
	gwBilling  gwRoute = "billing"
	gwShipping gwRoute = "shipping"
)

// gatewayServer starts an in-memory server and a client that speaks the
// Vercel AI Gateway dialect to it.
func gatewayServer(t *testing.T, handler http.HandlerFunc, opts ...jev.Option) *jev.Client {
	t.Helper()
	srv := httptest.NewTestServer(t, handler)
	base := []jev.Option{
		jev.WithVercelAIGateway(),
		jev.WithAPIKey("vck-test-secret"),
		jev.WithBaseURL("http://gateway.test/v4/ai"),
		jev.WithHTTPClient(srv.Client()),
	}
	client, err := jev.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestVercelGatewayRequestAndResponse(t *testing.T) {
	var (
		mu   sync.Mutex
		req  *http.Request
		body []byte
	)
	client := gatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		req = r.Clone(context.Background())
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("x-vercel-id", "iad1::abc")
		io.WriteString(w, gatewayOKBody)
	}, jev.WithHeader("X-Tenant", "acme"))

	b := client.Batch(map[string]any{"ticket": "charged twice"}).Extra(map[string]any{"providerOptions": map[string]any{"gateway": map[string]any{"zeroDataRetention": true}}})
	spam := b.Add("spam", jev.Noul("Is this spam?", jev.NoulCriteria{True: "ads", False: "real"}))
	route := b.Add("route", jev.Choice("Route it.", jev.Opt(gwBilling, "charges"), jev.Opt(gwShipping, nil)))
	tone := b.Add("tone", jev.Score("How angry?", "calm", "annoyed", "furious"))
	resp, err := b.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if s, err := spam.Get(); err != nil || s.P != 0.02 {
		t.Errorf("spam = %+v, %v", s, err)
	}
	if r, err := route.Get(); err != nil || r.Value != gwBilling || r.Probs[gwShipping] != 0.1 || r.Confidence != 0.8 {
		t.Errorf("route = %+v, %v", r, err)
	}
	if s, err := tone.Get(); err != nil || s.Value != 1.5 || s.Probs[2] != 0.5 || s.Confidence != 0.5 {
		t.Errorf("tone = %+v, %v", s, err)
	}
	if resp.Model != "typesafe-ai/jev" || resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 3 {
		t.Errorf("response = %+v", resp)
	}
	if resp.RequestID != "iad1::abc" || resp.StatusCode != 200 || resp.Attempts != 1 {
		t.Errorf("meta = %+v", resp.Meta)
	}

	mu.Lock()
	defer mu.Unlock()
	if req.Method != http.MethodPost || req.URL.Path != "/v4/ai/evaluation-model" {
		t.Errorf("%s %s", req.Method, req.URL.Path)
	}
	for name, want := range map[string]string{
		"Authorization":                             "Bearer vck-test-secret",
		"Content-Type":                              "application/json",
		"Ai-Gateway-Protocol-Version":               "0.0.1",
		"Ai-Gateway-Auth-Method":                    "api-key",
		"Ai-Evaluation-Model-Specification-Version": "4",
		"Ai-Model-Id":                               "typesafe-ai/jev",
		"User-Agent":                                "jev-sdk-go/" + jev.Version,
		"X-Tenant":                                  "acme",
		"X-Typesafe-Sdk":                            "",
		"X-Typesafe-Retry-Count":                    "",
	} {
		if got := req.Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if _, has := got["model"]; has {
		t.Errorf("model must travel in the header, not the body: %s", body)
	}
	if !strings.Contains(string(body), `"spam":{"type":"boolean","instructions":"Is this spam?","criteria":{"true":"ads","false":"real"}}`) {
		t.Errorf("noul must be sent as boolean: %s", body)
	}
	if !strings.Contains(string(body), `"route":{"type":"choice","instructions":"Route it.","criteria":{"billing":"charges","shipping":null}}`) {
		t.Errorf("choice criteria: %s", body)
	}
	if !strings.Contains(string(body), `"tone":{"type":"score","instructions":"How angry?","criteria":["calm","annoyed","furious"]}`) {
		t.Errorf("score criteria: %s", body)
	}
	if !strings.Contains(string(body), `"providerOptions":{"gateway":{"zeroDataRetention":true}}`) {
		t.Errorf("extra fields must be top-level: %s", body)
	}
	if !strings.Contains(string(body), `"state":{"ticket":"charged twice"}`) {
		t.Errorf("state: %s", body)
	}
}

func TestVercelGatewayModelOverride(t *testing.T) {
	var mu sync.Mutex
	var modelHeader string
	client := gatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		modelHeader = r.Header.Get("Ai-Model-Id")
		mu.Unlock()
		io.WriteString(w, `{"answers":{"q":{"type":"boolean","probability":1}}}`)
	}, jev.WithModel("typesafe-ai/jev-1.13"))
	b := client.Batch("s")
	b.Add("q", jev.Noul("?"))
	resp, err := b.Run(t.Context())
	if err != nil || resp.Model != "typesafe-ai/jev-1.13" || resp.Usage != (jev.Usage{}) {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if modelHeader != "typesafe-ai/jev-1.13" {
		t.Errorf("Ai-Model-Id = %q", modelHeader)
	}
}

func TestVercelGatewayErrorsAndRetries(t *testing.T) {
	t.Run("gateway error body", func(t *testing.T) {
		client := gatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("x-vercel-id", "iad1::err")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"Invalid API key","type":"authentication_error","code":401}}`)
		})
		_, err := client.Ask(t.Context(), "s", jev.Noul("?"))
		var apiErr *jev.APIError
		if !errors.As(err, &apiErr) || !errors.Is(err, jev.ErrAuthentication) {
			t.Fatalf("err = %v", err)
		}
		if apiErr.RequestID != "iad1::err" || apiErr.Message != "Invalid API key" || apiErr.Attempts != 1 {
			t.Errorf("apiErr = %+v", apiErr)
		}
	})
	t.Run("rate limit is retried", func(t *testing.T) {
		var calls int
		var mu sync.Mutex
		client := gatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			io.WriteString(w, `{"answers":{"q":{"type":"boolean","probability":0.5}}}`)
		}, jev.WithRetryPolicy(jev.RetryPolicy{MaxRetries: 1}))
		a, err := client.Ask(t.Context(), "s", jev.Noul("?"))
		if err != nil || a.P != 0.5 {
			t.Fatalf("a = %+v, %v", a, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if calls != 2 {
			t.Errorf("calls = %d", calls)
		}
	})
	t.Run("missing answers is invalid response", func(t *testing.T) {
		client := gatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"usage":{"inputTokens":1}}`)
		})
		_, err := client.Ask(t.Context(), "s", jev.Noul("?"))
		if !errors.Is(err, jev.ErrInvalidResponse) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown answer kind is kept raw", func(t *testing.T) {
		client := gatewayServer(t, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"answers":{"q":{"type":"rank","order":["b","a"],"score":0.3}}}`)
		})
		raw, err := client.Ask(t.Context(), "s", jev.Raw(jev.Spec{Type: "rank", Instructions: "?"}))
		if err != nil || raw.Type != "rank" || string(raw.Extra["order"]) != `["b","a"]` || string(raw.Extra["score"]) != `0.3` {
			t.Errorf("raw = %+v, %v", raw, err)
		}
	})
	t.Run("models are not listed", func(t *testing.T) {
		client := gatewayServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not call the server") })
		if _, err := client.Models(t.Context()); !errors.Is(err, jev.ErrNoModelList) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestVercelGatewayConfiguration(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "sk-typesafe")
	t.Setenv("TYPESAFE_DEFAULT_MODEL", "jev-1.13")
	t.Setenv("AI_GATEWAY_API_KEY", "")
	if _, err := jev.New(jev.WithVercelAIGateway()); !errors.Is(err, jev.ErrNoAPIKey) || !strings.Contains(err.Error(), "AI_GATEWAY_API_KEY") {
		t.Errorf("without gateway key: err = %v", err)
	}
	t.Setenv("AI_GATEWAY_API_KEY", "vck-env")
	client, err := jev.New(jev.WithVercelAIGateway())
	if err != nil {
		t.Fatal(err)
	}
	if client.DefaultModel() != jev.DefaultVercelAIGatewayModel {
		t.Errorf("model = %q: TYPESAFE_DEFAULT_MODEL must not leak into the gateway", client.DefaultModel())
	}
}
