package jev

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Errors raised before a request is sent.
var (
	// ErrNoAPIKey means the client had no credential: nothing was passed and
	// TYPESAFE_API_KEY held nothing either.
	ErrNoAPIKey = errors.New("jev: no API key: set TYPESAFE_API_KEY or use WithAPIKey")
	// ErrInvalidConfig wraps every configuration error from [New].
	ErrInvalidConfig = errors.New("jev: invalid configuration")
	// ErrInvalidRequest means a question or request could not have been
	// answered as written, such as a choice with no options. Nothing is sent.
	ErrInvalidRequest = errors.New("jev: invalid request")
	// ErrNotRun means a [Handle] was read before its [Batch] ran.
	ErrNotRun = errors.New("jev: batch has not run")
	// ErrBatchUsed means a [Batch] was run twice or added to after running.
	ErrBatchUsed = errors.New("jev: batch already run")
	// ErrNoModelList means the client's provider cannot list models.
	ErrNoModelList = errors.New("jev: provider does not list models")
)

// Errors raised after a successful HTTP response.
var (
	// ErrMalformedAnswer means an answer does not fit its question: missing,
	// the wrong kind, or naming an option that was never offered.
	ErrMalformedAnswer = errors.New("jev: malformed answer")
	// ErrInvalidResponse means a 2xx body does not match the API contract.
	// Every [*ResponseError] matches it.
	ErrInvalidResponse = errors.New("jev: invalid response")
	// ErrResponseTooLarge is the cause of a [*ResponseError] for a body over
	// 16 MiB. It is never retried.
	ErrResponseTooLarge = errors.New("jev: response body exceeds 16 MiB")
)

// Errors raised by the transport, below HTTP. Every [*ConnectionError]
// matches [ErrConnection]; one caused by a timeout also matches [ErrTimeout].
var (
	ErrConnection = errors.New("jev: connection error")
	ErrTimeout    = errors.New("jev: request timed out")
)

// Errors matching an API status, one per status the official SDKs
// distinguish. A [*APIError] matches them through [errors.Is].
var (
	ErrBadRequest       = errors.New("jev: bad request")           // HTTP 400
	ErrAuthentication   = errors.New("jev: authentication failed") // HTTP 401
	ErrPermissionDenied = errors.New("jev: permission denied")     // HTTP 403
	ErrNotFound         = errors.New("jev: not found")             // HTTP 404
	ErrUnprocessable    = errors.New("jev: unprocessable entity")  // HTTP 422
	ErrRateLimit        = errors.New("jev: rate limited")          // HTTP 429
	ErrOverloaded       = errors.New("jev: overloaded")            // HTTP 529
	ErrInternalServer   = errors.New("jev: internal server error") // HTTP 5xx, including 529
)

// StatusOverloaded is 529, which TypeSafe returns when the service is
// briefly past capacity.
const StatusOverloaded = 529

// maxMessageLength matches the official SDKs' MAX_ERROR_BODY_LENGTH.
const maxMessageLength = 200

// FieldError is one entry of a validation failure, as the API reports it on
// a 422 response.
type FieldError struct {
	// Type is the kind of violation, such as "missing" or "too_short".
	Type string `json:"type"`
	// Loc is the path to the offending field, such as ["body", "model"].
	Loc []any `json:"loc"`
	// Msg is the human-readable explanation.
	Msg string `json:"msg"`
}

// String renders the entry as "path: message", dropping the leading "body"
// segment, as the official SDKs do.
func (f FieldError) String() string {
	path := make([]string, 0, len(f.Loc))
	for _, part := range f.Loc {
		if s := fmt.Sprint(part); s != "body" {
			path = append(path, s)
		}
	}
	if len(path) == 0 {
		return f.Msg
	}
	return strings.Join(path, ".") + ": " + f.Msg
}

// APIError is an unsuccessful HTTP response, returned after any retries.
// It matches the status sentinels through [errors.Is]:
//
//	if errors.Is(err, jev.ErrRateLimit) { ... }
type APIError struct {
	StatusCode int
	// Endpoint is the method and URL of the request.
	Endpoint string
	Header   http.Header
	// Body is the raw response body, or nil when empty.
	Body []byte
	// Message is the server's explanation extracted from Body, or the body
	// itself, truncated. It can contain anything the server reflected.
	Message string
	// Type is the API's error_type, such as "authentication_error", when
	// the body carried one.
	Type string
	// Fields lists the offending fields of a validation failure.
	Fields []FieldError
	// RequestID is the x-typesafe-request-id header, or empty when absent.
	RequestID string
	// RetryAfter is the delay the server asked for, from retry-after-ms or
	// Retry-After. It is zero when the response carried neither.
	RetryAfter time.Duration
	// Attempts is the number of HTTP attempts made, including retries.
	Attempts int
}

func newAPIError(res result) *APIError {
	e := &APIError{
		StatusCode: res.status,
		Endpoint:   res.method + " " + res.url,
		Header:     res.header,
		Body:       res.body,
		RequestID:  res.header.Get(requestIDHeader),
		RetryAfter: parseRetryAfter(res.header, time.Now()),
		Attempts:   res.attempts,
	}
	e.parseBody()
	return e
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "jev: %s: %d", e.Endpoint, e.StatusCode)
	if text := http.StatusText(e.StatusCode); text != "" {
		b.WriteString(" " + text)
	}
	if detail := e.detail(); detail != "" {
		b.WriteString(" " + detail)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request_id=%s)", e.RequestID)
	}
	return b.String()
}

// Is reports whether target is a status sentinel for this error. A status
// may match more than one: 529 matches both [ErrOverloaded] and
// [ErrInternalServer].
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrBadRequest:
		return e.StatusCode == http.StatusBadRequest
	case ErrAuthentication:
		return e.StatusCode == http.StatusUnauthorized
	case ErrPermissionDenied:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrUnprocessable:
		return e.StatusCode == http.StatusUnprocessableEntity
	case ErrRateLimit:
		return e.StatusCode == http.StatusTooManyRequests
	case ErrOverloaded:
		return e.StatusCode == StatusOverloaded
	case ErrInternalServer:
		return e.StatusCode >= 500
	}
	return false
}

func (e *APIError) detail() string {
	switch {
	case len(e.Fields) > 0:
		parts := make([]string, len(e.Fields))
		for i, f := range e.Fields {
			parts[i] = f.String()
		}
		return truncate(strings.Join(parts, "; "))
	case e.Message != "":
		if e.Type != "" {
			return truncate(e.Type + ": " + e.Message)
		}
		return truncate(e.Message)
	case len(e.Body) == 0:
		return "(no body)"
	}
	return truncate(string(e.Body))
}

// parseBody fills Type, Message, and Fields following the official SDKs'
// extract_message order: "error" as a string or object, then "message",
// then "detail" as a string, object, or list of validation errors.
func (e *APIError) parseBody() {
	var body any
	if json.Unmarshal(e.Body, &body) != nil {
		e.Message = strings.TrimSpace(string(e.Body))
		return
	}
	switch v := body.(type) {
	case string:
		e.Message = v
	case map[string]any:
		if e.readAlternative(v["error"]) {
			return
		}
		if s, ok := v["message"].(string); ok && s != "" {
			e.Message = s
			return
		}
		e.readAlternative(v["detail"])
	}
}

func (e *APIError) readAlternative(raw any) bool {
	switch v := raw.(type) {
	case string:
		e.Message = v
		return v != ""
	case map[string]any:
		e.Type, _ = v["error_type"].(string)
		e.Message, _ = v["message"].(string)
		return e.Type != "" || e.Message != ""
	case []any:
		for _, entry := range v {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			msg, ok := m["msg"].(string)
			if !ok || msg == "" {
				continue
			}
			loc, _ := m["loc"].([]any)
			typ, _ := m["type"].(string)
			e.Fields = append(e.Fields, FieldError{Type: typ, Loc: loc, Msg: msg})
		}
		return len(e.Fields) > 0
	}
	return false
}

// ConnectionError is a request that failed without a complete HTTP
// response, returned after any retries.
type ConnectionError struct {
	Endpoint string
	// Elapsed is how long the failed attempt, or the whole call for a
	// [RetryPolicy.TotalTimeout], ran.
	Elapsed time.Duration
	// Timeout is the SDK limit that elapsed: the per-attempt timeout or the
	// total timeout. It is zero when the failure was not an SDK timeout.
	Timeout time.Duration
	// StatusCode, Header, and RequestID are set when the response headers
	// arrived before the body failed.
	StatusCode int
	Header     http.Header
	RequestID  string
	Attempts   int
	Err        error
}

func (e *ConnectionError) Error() string {
	switch {
	case e.Timeout > 0:
		return fmt.Sprintf("jev: %s: timed out after %v (limit %v)", e.Endpoint, e.Elapsed, e.Timeout)
	case isNetTimeout(e.Err):
		return fmt.Sprintf("jev: %s: timed out after %v: %v", e.Endpoint, e.Elapsed, e.Err)
	}
	return fmt.Sprintf("jev: %s: connection error after %v: %v", e.Endpoint, e.Elapsed, e.Err)
}

func (e *ConnectionError) Unwrap() error { return e.Err }

// Is reports whether target is [ErrConnection] or, for a timeout, [ErrTimeout].
func (e *ConnectionError) Is(target error) bool {
	switch target {
	case ErrConnection:
		return true
	case ErrTimeout:
		return e.Timeout > 0 || isNetTimeout(e.Err)
	}
	return false
}

func isNetTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// ResponseError is a successful HTTP response whose body does not match the
// API contract. It matches [ErrInvalidResponse] and is never retried.
type ResponseError struct {
	Endpoint string
	// Path is the dotted path of the offending field, when known.
	Path      string
	Header    http.Header
	Body      []byte
	RequestID string
	Attempts  int
	Err       error
}

func newResponseError(res result, path string, err error) *ResponseError {
	return &ResponseError{
		Endpoint:  res.method + " " + res.url,
		Path:      path,
		Header:    res.header,
		Body:      res.body,
		RequestID: res.header.Get(requestIDHeader),
		Attempts:  res.attempts,
		Err:       err,
	}
}

func (e *ResponseError) Error() string {
	msg := fmt.Sprintf("jev: %s: invalid response", e.Endpoint)
	if e.Path != "" {
		msg += fmt.Sprintf(" at %q", e.Path)
	}
	msg += ": " + e.Err.Error()
	if e.RequestID != "" {
		msg += fmt.Sprintf(" (request_id=%s)", e.RequestID)
	}
	return msg
}

func (e *ResponseError) Unwrap() error { return e.Err }

// Is reports whether target is [ErrInvalidResponse].
func (e *ResponseError) Is(target error) bool { return target == ErrInvalidResponse }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidRequest}, args...)...)
}

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrMalformedAnswer}, args...)...)
}

// truncate keeps at most maxMessageLength characters, cutting on a rune
// boundary.
func truncate(s string) string {
	count := 0
	for i := range s {
		if count == maxMessageLength {
			return s[:i] + "…"
		}
		count++
	}
	return s
}

// parseRetryAfter reads retry-after-ms first, then Retry-After as seconds or
// an HTTP date. It returns zero when neither carries a usable delay.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	if raw := strings.TrimSpace(h.Get(retryAfterMSHeader)); raw != "" {
		if ms, err := strconv.ParseFloat(raw, 64); err == nil && ms >= 0 && !math.IsInf(ms, 0) {
			return durationOf(ms * float64(time.Millisecond))
		}
	}
	raw := strings.TrimSpace(h.Get(retryAfterHeader))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		if secs < 0 || math.IsInf(secs, 0) || math.IsNaN(secs) {
			return 0
		}
		return durationOf(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(raw); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}

func durationOf(nanoseconds float64) time.Duration {
	if nanoseconds >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(nanoseconds)
}
