package jev

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// Defaults, matching the official TypeSafe SDKs.
const (
	DefaultBaseURL = "https://api.typesafe.ai"
	DefaultModel   = "jev-latest"
	// DefaultTimeout bounds each HTTP attempt. See
	// [RetryPolicy.TotalTimeout] for the whole-call budget.
	DefaultTimeout = 10 * time.Second
)

// Environment variables read when the matching option is not given. Blank
// values are ignored.
const (
	EnvAPIKey       = "TYPESAFE_API_KEY"
	EnvBaseURL      = "TYPESAFE_BASE_URL"
	EnvDefaultModel = "TYPESAFE_DEFAULT_MODEL"
	EnvLogLevel     = "TYPESAFE_LOG_LEVEL"
)

// Client evaluates questions through a [Provider]. Create it with [New]; it
// is safe for concurrent use.
type Client struct {
	provider Provider
	lister   ModelLister
	model    string
}

type config struct {
	apiKey     string
	apiKeySet  bool
	baseURL    string
	model      string
	userAgent  string
	headers    http.Header
	httpClient *http.Client
	timeout    time.Duration
	retry      *RetryPolicy
	logger     *slog.Logger
	provider   Provider
	middleware []Middleware
}

// Option configures a [Client]. Options apply in order, last wins.
type Option func(*config)

// WithAPIKey sets the API key. Once it is given, TYPESAFE_API_KEY is not
// read at all, so an empty key is [ErrNoAPIKey] rather than a silent fall
// back to whatever the environment holds. This is what the official SDKs do.
func WithAPIKey(key string) Option {
	return func(c *config) { c.apiKey, c.apiKeySet = strings.TrimSpace(key), true }
}

// WithBaseURL sets the API root, for proxies and tests. It must be an http
// or https URL without credentials, query, or fragment. Defaults to
// TYPESAFE_BASE_URL, then [DefaultBaseURL].
func WithBaseURL(baseURL string) Option {
	return func(c *config) { c.baseURL = strings.TrimSpace(baseURL) }
}

// WithModel sets the model used when a batch does not name one. Defaults to
// TYPESAFE_DEFAULT_MODEL, then [DefaultModel]. [Batch.Model] overrides it
// per batch.
func WithModel(model string) Option {
	return func(c *config) { c.model = strings.TrimSpace(model) }
}

// WithTimeout bounds each HTTP attempt, including reading the body. It must
// be positive. Defaults to [DefaultTimeout].
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithRetryPolicy replaces the retry policy. See [DefaultRetryPolicy].
func WithRetryPolicy(policy RetryPolicy) Option {
	return func(c *config) { c.retry = new(policy) }
}

// WithMaxRetries changes only the retry count, keeping the rest of the
// current policy. Zero disables retries.
func WithMaxRetries(n int) Option {
	return func(c *config) {
		policy := DefaultRetryPolicy()
		if c.retry != nil {
			policy = *c.retry
		}
		policy.MaxRetries = n
		c.retry = new(policy)
	}
}

// WithHeader sets a header sent with every request, replacing any earlier
// value. Authorization, Accept, Content-Type, and the SDK identification
// headers cannot be overridden.
func WithHeader(key, value string) Option {
	return func(c *config) {
		if c.headers == nil {
			c.headers = http.Header{}
		}
		c.headers.Set(key, value)
	}
}

// WithUserAgent replaces the User-Agent header. The X-TypeSafe-SDK header
// keeps identifying this SDK.
func WithUserAgent(ua string) Option {
	return func(c *config) { c.userAgent = strings.TrimSpace(ua) }
}

// WithHTTPClient supplies the [http.Client] used for requests, which is
// where to install a custom or instrumented transport. The client is never
// modified; per-attempt timeouts are applied through the request context.
// Defaults to [http.DefaultClient].
func WithHTTPClient(hc *http.Client) Option {
	return func(c *config) { c.httpClient = hc }
}

// WithLogger sets the logger: one record per HTTP attempt at
// [slog.LevelInfo], and headers and bodies at [slog.LevelDebug]. Credential
// headers are redacted; bodies, which contain your state and answers, are
// not. Without a logger, TYPESAFE_LOG_LEVEL selects a level on
// [slog.Default]; unset means no logging.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) { c.logger = l }
}

// WithProvider replaces the HTTP provider, for fakes or alternative
// backends. No API key is required when a provider is supplied.
func WithProvider(p Provider) Option {
	return func(c *config) { c.provider = p }
}

// WithMiddleware wraps the provider. The first middleware given is
// outermost. Middleware does not affect [Client.Models].
func WithMiddleware(m ...Middleware) Option {
	return func(c *config) { c.middleware = append(c.middleware, m...) }
}

// New builds a client. Explicit options win over environment variables,
// which win over the defaults. Unless [WithProvider] redirects it, the
// client talks to the TypeSafe API and needs a key; without one it fails
// with [ErrNoAPIKey] here rather than at the first request.
func New(opts ...Option) (*Client, error) {
	cfg := config{userAgent: userAgent, timeout: DefaultTimeout}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.model == "" {
		cfg.model = env(EnvDefaultModel)
	}
	if cfg.model == "" {
		cfg.model = DefaultModel
	}

	base := cfg.provider
	if base == nil {
		var err error
		if base, err = cfg.buildHTTPProvider(); err != nil {
			return nil, err
		}
	}
	// Captured before wrapping, so middleware cannot hide the models endpoint.
	lister, _ := base.(ModelLister)

	provider := base
	for _, m := range slices.Backward(cfg.middleware) {
		provider = m(provider)
	}
	return &Client{provider: provider, lister: lister, model: cfg.model}, nil
}

func (cfg *config) buildHTTPProvider() (*httpProvider, error) {
	key := cfg.apiKey
	if !cfg.apiKeySet {
		key = env(EnvAPIKey)
	}
	if key == "" {
		return nil, ErrNoAPIKey
	}
	if cfg.baseURL == "" {
		cfg.baseURL = env(EnvBaseURL)
	}
	if cfg.baseURL == "" {
		cfg.baseURL = DefaultBaseURL
	}
	baseURL, err := parseBaseURL(cfg.baseURL)
	if err != nil {
		return nil, err
	}
	if cfg.timeout <= 0 {
		return nil, fmt.Errorf("%w: timeout must be positive, got %v", ErrInvalidConfig, cfg.timeout)
	}
	if cfg.userAgent == "" {
		return nil, fmt.Errorf("%w: user agent must not be empty", ErrInvalidConfig)
	}
	retry := DefaultRetryPolicy()
	if cfg.retry != nil {
		retry = *cfg.retry
		if err := retry.validate(); err != nil {
			return nil, err
		}
	}
	logger := cfg.logger
	if logger == nil {
		if logger, err = loggerFromEnv(); err != nil {
			return nil, err
		}
	}
	hc := cfg.httpClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return &httpProvider{
		baseURL:   baseURL,
		apiKey:    key,
		userAgent: cfg.userAgent,
		headers:   cfg.headers.Clone(),
		hc:        hc,
		timeout:   cfg.timeout,
		retry:     retry,
		logger:    logger,
	}, nil
}

// parseBaseURL rejects anything that could leak into logs or break path
// concatenation, without echoing the offending value.
func parseBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: invalid base URL", ErrInvalidConfig)
	}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return "", fmt.Errorf("%w: base URL scheme must be http or https", ErrInvalidConfig)
	case parsed.Host == "":
		return "", fmt.Errorf("%w: base URL needs a host", ErrInvalidConfig)
	case parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery:
		return "", fmt.Errorf("%w: base URL must not contain credentials, a query, or a fragment", ErrInvalidConfig)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func env(name string) string { return strings.TrimSpace(os.Getenv(name)) }

// DefaultModel returns the model used when a batch does not name one.
func (c *Client) DefaultModel() string { return c.model }

// Models lists the models the account may use, through GET /v1/models. It
// needs a provider that implements [ModelLister], which the default HTTP
// provider does; otherwise it returns [ErrNoModelList].
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	if c.lister == nil {
		return nil, ErrNoModelList
	}
	return c.lister.Models(ctx)
}
