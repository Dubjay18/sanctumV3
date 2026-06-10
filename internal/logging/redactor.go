package logging

import (
	"context"
	"log/slog"
)

// RedactingHandler wraps another slog.Handler to redact sensitive fields.
type RedactingHandler struct {
	handler slog.Handler
}

// NewRedactingHandler creates a new RedactingHandler.
func NewRedactingHandler(h slog.Handler) *RedactingHandler {
	return &RedactingHandler{handler: h}
}

// Enabled forwards the call to the wrapped handler.
func (h *RedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

// redactAttr replaces sensitive fields with "[REDACTED]".
func redactAttr(attr slog.Attr) slog.Attr {
	name := attr.Key
	if name == "private_key" || name == "api_key" || name == "password" || name == "token" {
		return slog.String(name, "[REDACTED]")
	}

	if attr.Value.Kind() == slog.KindGroup {
		attrs := attr.Value.Group()
		newAttrs := make([]slog.Attr, len(attrs))
		for i, a := range attrs {
			newAttrs[i] = redactAttr(a)
		}
		return slog.Attr{Key: name, Value: slog.GroupValue(newAttrs...)}
	}

	return attr
}

// Handle clones the record and redacts all attributes.
func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	newRecord := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(attr slog.Attr) bool {
		newRecord.AddAttrs(redactAttr(attr))
		return true
	})
	return h.handler.Handle(ctx, newRecord)
}

// WithAttrs returns a handler with redacted attributes.
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		redacted[i] = redactAttr(attr)
	}
	return &RedactingHandler{handler: h.handler.WithAttrs(redacted)}
}

// WithGroup returns a handler with a new group name.
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{handler: h.handler.WithGroup(name)}
}
