package postgres

import (
	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
)

// LoggerAdapter routes platform-pgcommon's logging (slow queries, migration
// output) into Billing's logger.
type LoggerAdapter struct {
	log port.Logger
}

var _ domain.Logger = LoggerAdapter{}

// NewLoggerAdapter wraps log as a pgcommon domain.Logger.
func NewLoggerAdapter(log port.Logger) LoggerAdapter { return LoggerAdapter{log: log} }

// Debug logs at debug level.
func (a LoggerAdapter) Debug(msg string, fields ...domain.Field) { a.log.Debug(msg, fieldMap(fields)) }

// Info logs at info level.
func (a LoggerAdapter) Info(msg string, fields ...domain.Field) { a.log.Info(msg, fieldMap(fields)) }

// Warn logs at warn level.
func (a LoggerAdapter) Warn(msg string, fields ...domain.Field) { a.log.Warn(msg, fieldMap(fields)) }

// Error logs at error level.
func (a LoggerAdapter) Error(msg string, fields ...domain.Field) { a.log.Error(msg, fieldMap(fields)) }

func fieldMap(fields []domain.Field) map[string]any {
	m := make(map[string]any, len(fields))
	for _, f := range fields {
		m[f.Key] = f.Value
	}
	return m
}
