package jev

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// loggerFromEnv builds the default logger from TYPESAFE_LOG_LEVEL: a level
// on [slog.Default], or a discarding logger when unset or "off".
func loggerFromEnv() (*slog.Logger, error) {
	raw := env(EnvLogLevel)
	if raw == "" {
		return slog.New(slog.DiscardHandler), nil
	}
	var level slog.Level
	switch strings.ToLower(raw) {
	case "off":
		return slog.New(slog.DiscardHandler), nil
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("%w: invalid %s %q: expected debug, info, warn, error, or off",
			ErrInvalidConfig, EnvLogLevel, raw)
	}
	return slog.New(&levelHandler{Handler: slog.Default().Handler(), level: level}), nil
}

type levelHandler struct {
	slog.Handler
	level slog.Level
}

func (h *levelHandler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.level }

func (h *levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelHandler{Handler: h.Handler.WithAttrs(attrs), level: h.level}
}

func (h *levelHandler) WithGroup(name string) slog.Handler {
	return &levelHandler{Handler: h.Handler.WithGroup(name), level: h.level}
}

// redactHeaderValue matches the official SDKs: credential headers keep the
// scheme and the last four characters of long secrets; cookies and headers
// named like tokens or secrets are fully masked.
func redactHeaderValue(name, value string) string {
	switch lower := strings.ToLower(name); {
	case lower == "authorization", lower == "proxy-authorization", lower == "x-api-key", lower == "api-key":
		return redactSecret(value)
	case lower == "cookie", lower == "set-cookie", strings.Contains(lower, "token"), strings.Contains(lower, "secret"):
		return "***"
	}
	return value
}

func redactSecret(value string) string {
	scheme, secret, found := strings.Cut(value, " ")
	if !found {
		scheme, secret = "", value
	}
	tail := ""
	if len(secret) > 8 {
		tail = secret[len(secret)-4:]
	}
	if scheme != "" {
		return scheme + " ***" + tail
	}
	return "***" + tail
}

func redactedHeaders(headers http.Header) slog.Value {
	attrs := make([]slog.Attr, 0, len(headers))
	for name, values := range headers {
		redacted := make([]string, len(values))
		for i, value := range values {
			redacted[i] = redactHeaderValue(name, value)
		}
		attrs = append(attrs, slog.Any(name, redacted))
	}
	return slog.GroupValue(attrs...)
}
