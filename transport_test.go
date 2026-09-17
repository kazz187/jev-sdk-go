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
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kazz187/jev-sdk-go"
)

const okBody = `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.7}},"usage":{"input_tokens":3,"output_tokens":1}}`

// server starts an in-memory test server, usable inside a synctest bubble,
// and a client pointed at it with the given options.
func server(t *testing.T, handler http.HandlerFunc, opts ...jev.Option) *jev.Client {
	t.Helper()
	srv := httptest.NewTestServer(t, handler)
	base := []jev.Option{
		jev.WithAPIKey("sk-test-secret-key"),
		jev.WithBaseURL("http://jev.test"),
		jev.WithHTTPClient(srv.Client()),
	}
	client, err := jev.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestHTTPRequestAndResponse(t *testing.T) {
	var (
		mu   sync.Mutex
		req  *http.Request
		body []byte
	)
	client := server(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		req = r.Clone(context.Background())
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("x-typesafe-request-id", "req_123")
		io.WriteString(w, okBody)
	}, jev.WithHeader("X-Tenant", "acme"), jev.WithHeader("Authorization", "nope"))

	b := client.Batch("hello")
	h := b.Add("q", jev.Noul("Is it a greeting?"))
	resp, err := b.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.Get()
	if err != nil || a.P != 0.7 {
		t.Errorf("answer = %v, %v", a, err)
	}
	if resp.RequestID != "req_123" || resp.StatusCode != 200 || resp.Attempts != 1 || resp.Latency <= 0 || resp.Header.Get("X-Typesafe-Request-Id") != "req_123" {
		t.Errorf("meta = %+v", resp.Meta)
	}
	if resp.Model != "jev-1.13.0" || resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 1 {
		t.Errorf("response = %+v", resp)
	}

	mu.Lock()
	defer mu.Unlock()
	if req.Method != http.MethodPost || req.URL.Path != "/v1/systemone" {
		t.Errorf("%s %s", req.Method, req.URL.Path)
	}
	for name, want := range map[string]string{
		"Authorization":          "Bearer sk-test-secret-key",
		"Content-Type":           "application/json",
		"Accept":                 "application/json",
		"User-Agent":             "jev-sdk-go/" + jev.Version,
		"X-Typesafe-Sdk":         "jev-sdk-go/" + jev.Version,
		"X-Tenant":               "acme",
		"X-Typesafe-Retry-Count": "",
	} {
		if got := req.Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	if !strings.HasPrefix(req.Header.Get("X-Typesafe-Runtime"), "go/") {
		t.Errorf("runtime header = %q", req.Header.Get("X-Typesafe-Runtime"))
	}
	want := `{"model":"jev-latest","state":"hello","questions":{"q":{"type":"noul","instructions":"Is it a greeting?"}}}`
	if string(body) != want {
		t.Errorf("body = %s", body)
	}
}

func TestRetriesOverloadedThenSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		var retryHeaders []string
		var mu sync.Mutex
		client := server(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			retryHeaders = append(retryHeaders, r.Header.Get("X-Typesafe-Retry-Count"))
			mu.Unlock()
			switch calls.Add(1) {
			case 1:
				w.Header().Set("retry-after-ms", "1500")
				w.WriteHeader(jev.StatusOverloaded)
			case 2:
				w.WriteHeader(http.StatusInternalServerError)
			default:
				io.WriteString(w, okBody)
			}
		})
		start := time.Now()
		b := client.Batch("s")
		b.Add("q", jev.Noul("?"))
		resp, err := b.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if resp.Attempts != 3 || calls.Load() != 3 {
			t.Errorf("attempts = %d, calls = %d", resp.Attempts, calls.Load())
		}
		// 1.5s from retry-after-ms, then 1s backoff minus up to 25% jitter.
		if elapsed := time.Since(start); elapsed < 2250*time.Millisecond || elapsed > 2500*time.Millisecond {
			t.Errorf("elapsed = %v, want retry-after 1.5s + backoff", elapsed)
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.Join(retryHeaders, ",") != ",1,2" {
			t.Errorf("retry count headers = %q", retryHeaders)
		}
	})
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			client := server(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("x-typesafe-request-id", "req_err")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"error_type":"some_error","message":"nope"}}`)
			})
			_, err := client.Ask(t.Context(), "s", jev.Noul("?"))
			var apiErr *jev.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != status || calls.Load() != 1 {
				t.Fatalf("err = %v, calls = %d", err, calls.Load())
			}
			if apiErr.RequestID != "req_err" || apiErr.Type != "some_error" || apiErr.Message != "nope" || apiErr.Attempts != 1 {
				t.Errorf("apiErr = %+v", apiErr)
			}
			if !strings.Contains(err.Error(), "some_error: nope") || !strings.Contains(err.Error(), "request_id=req_err") {
				t.Errorf("Error() = %q", err)
			}
		})
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		client := server(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
		}, jev.WithMaxRetries(1))
		_, err := client.Ask(t.Context(), "s", jev.Noul("?"))
		if !errors.Is(err, jev.ErrRateLimit) || calls.Load() != 2 {
			t.Errorf("err = %v, calls = %d", err, calls.Load())
		}
	})
}

func TestPerAttemptTimeoutIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		client := server(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				waitForCancel(r)
				return
			}
			io.WriteString(w, okBody)
		}, jev.WithTimeout(2*time.Second))
		start := time.Now()
		a, err := client.Ask(t.Context(), "s", jev.Noul("?"))
		if err != nil || a.P != 0.7 || calls.Load() != 2 {
			t.Fatalf("err = %v, calls = %d", err, calls.Load())
		}
		if elapsed := time.Since(start); elapsed < 2*time.Second {
			t.Errorf("elapsed = %v", elapsed)
		}
	})
}

func TestTimeoutWithoutRetriesIsAConnectionError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := server(t, func(w http.ResponseWriter, r *http.Request) {
			waitForCancel(r)
		}, jev.WithTimeout(time.Second), jev.WithMaxRetries(0))
		_, err := client.Ask(t.Context(), "s", jev.Noul("?"))
		var connErr *jev.ConnectionError
		if !errors.As(err, &connErr) || !errors.Is(err, jev.ErrTimeout) || !errors.Is(err, jev.ErrConnection) {
			t.Fatalf("err = %v", err)
		}
		if connErr.Timeout != time.Second || connErr.Attempts != 1 {
			t.Errorf("connErr = %+v", connErr)
		}
	})
}

func TestTotalTimeoutBoundsTheWholeCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		policy := jev.DefaultRetryPolicy()
		policy.TotalTimeout = 3 * time.Second
		policy.MaxRetries = 10
		var calls atomic.Int32
		client := server(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
		}, jev.WithRetryPolicy(policy))
		start := time.Now()
		_, err := client.Ask(t.Context(), "s", jev.Noul("?"))
		// First attempt fails, waits 2s, second fails; a third would start at
		// 4s, past the budget, so the last API error is returned.
		if !errors.Is(err, jev.ErrInternalServer) || calls.Load() != 2 {
			t.Errorf("err = %v, calls = %d", err, calls.Load())
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("elapsed = %v", elapsed)
		}
	})
}

func TestCallerCancellationWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := server(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(100 * time.Millisecond) // during the first backoff
			cancel()
		}()
		_, err := client.Ask(ctx, "s", jev.Noul("?"))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestResponseValidation(t *testing.T) {
	cases := map[string]string{
		"not json":   `nope`,
		"no model":   `{"answers":{},"usage":{}}`,
		"no answers": `{"model":"m","usage":{}}`,
		"wrong type": `{"model":"m","answers":[]}`,
		"dup names":  `{"model":"m","model":"n","answers":{}}`,
		"bad noul":   `{"model":"m","answers":{"q":{"type":"noul","noul":"high"}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			client := server(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.WriteString(w, body)
			})
			_, err := client.Ask(t.Context(), "s", jev.Noul("?"))
			var respErr *jev.ResponseError
			if !errors.As(err, &respErr) || !errors.Is(err, jev.ErrInvalidResponse) || calls.Load() != 1 {
				t.Fatalf("err = %v, calls = %d", err, calls.Load())
			}
		})
	}
	t.Run("too large", func(t *testing.T) {
		client := server(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "")
			io.Copy(w, io.LimitReader(zeros{}, 16<<20+1))
		})
		_, err := client.Ask(t.Context(), "s", jev.Noul("?"))
		if !errors.Is(err, jev.ErrResponseTooLarge) || !errors.Is(err, jev.ErrInvalidResponse) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown answer kind is kept raw", func(t *testing.T) {
		client := server(t, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"model":"m","answers":{"q":{"type":"rank","order":["b","a"]}},"usage":{}}`)
		})
		raw, err := client.Ask(t.Context(), "s", jev.Raw(jev.Spec{Type: "rank", Instructions: "?"}))
		if err != nil || raw.Type != "rank" || string(raw.Extra["order"]) != `["b","a"]` {
			t.Errorf("raw = %+v, %v", raw, err)
		}
	})
}

// waitForCancel blocks a handler until the client gives up, with a fake-time
// safety net so a failing test cannot deadlock the synctest bubble.
func waitForCancel(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(time.Minute):
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestModels(t *testing.T) {
	client := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Content-Type") != "" {
			t.Errorf("%s %s content-type=%q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		io.WriteString(w, `{"models":[{"name":"jev-latest","description":"alias","release_date":"2026-09-01"}]}`)
	})
	models, err := client.Models(t.Context())
	if err != nil || len(models) != 1 || models[0].ReleaseDate != "2026-09-01" {
		t.Fatalf("models = %v, %v", models, err)
	}
	fake, _ := jev.New(jev.WithProvider(jev.ProviderFunc(nil)))
	if _, err := fake.Models(t.Context()); !errors.Is(err, jev.ErrNoModelList) {
		t.Errorf("err = %v", err)
	}
}

func TestMiddlewareWrapsProviderButNotModels(t *testing.T) {
	var order []string
	tag := func(name string) jev.Middleware {
		return func(next jev.Provider) jev.Provider {
			return jev.ProviderFunc(func(ctx context.Context, r *jev.Request) (*jev.Response, error) {
				order = append(order, name)
				return next.Evaluate(ctx, r)
			})
		}
	}
	client := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"models":[]}`)
			return
		}
		io.WriteString(w, okBody)
	}, jev.WithMiddleware(tag("outer"), tag("inner")))
	if _, err := client.Ask(t.Context(), "s", jev.Noul("?")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ">") != "outer>inner" {
		t.Errorf("order = %v", order)
	}
	if _, err := client.Models(t.Context()); err != nil || len(order) != 2 {
		t.Errorf("Models through middleware: %v, order = %v", err, order)
	}
}

func TestClientIsSafeForConcurrentUse(t *testing.T) {
	client := server(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okBody)
	})
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := client.Ask(t.Context(), "s", jev.Noul("?")); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestStructuredStateRoundTrips(t *testing.T) {
	type Ticket struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	var got map[string]any
	client := server(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.UnmarshalRead(r.Body, &got); err != nil {
			t.Error(err)
		}
		io.WriteString(w, okBody)
	})
	if _, err := client.Ask(t.Context(), Ticket{"Refund", "Charged twice"}, jev.Noul("?")); err != nil {
		t.Fatal(err)
	}
	if state, _ := got["state"].(map[string]any); state["subject"] != "Refund" {
		t.Errorf("state = %v", got["state"])
	}
}
