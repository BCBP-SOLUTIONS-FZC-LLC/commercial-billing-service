package app

import (
	"context"
	"maps"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// ctxLogger adds the §11.4 correlation fields (trace_id, span_id) from ctx
// to every line. Redaction stays the callers' rule: no card data, no
// contact PII, no secrets (§10.1).
type ctxLogger struct {
	next   port.Logger
	fields map[string]any
}

// LoggerFor returns log with ctx's correlation fields (trace_id, span_id
// from the gincommon tracer) added; a nil log discards.
func LoggerFor(ctx context.Context, log port.Logger) port.Logger {
	if log == nil {
		return nopLogger{}
	}
	f := Tracer().LogFields(ctx)
	if len(f) == 0 {
		return log
	}
	return ctxLogger{next: log, fields: f}
}

func (l ctxLogger) with(f map[string]any) map[string]any {
	out := maps.Clone(l.fields)
	maps.Copy(out, f)
	return out
}

func (l ctxLogger) Debug(msg string, f map[string]any) { l.next.Debug(msg, l.with(f)) }
func (l ctxLogger) Info(msg string, f map[string]any)  { l.next.Info(msg, l.with(f)) }
func (l ctxLogger) Warn(msg string, f map[string]any)  { l.next.Warn(msg, l.with(f)) }
func (l ctxLogger) Error(msg string, f map[string]any) { l.next.Error(msg, l.with(f)) }

type nopLogger struct{}

func (nopLogger) Debug(string, map[string]any) {}
func (nopLogger) Info(string, map[string]any)  {}
func (nopLogger) Warn(string, map[string]any)  {}
func (nopLogger) Error(string, map[string]any) {}

// logger is the use cases' ctx-correlated logger.
func (s *Service) logger(ctx context.Context) port.Logger { return LoggerFor(ctx, s.Log) }
