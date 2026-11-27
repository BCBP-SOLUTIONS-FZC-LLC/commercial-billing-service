// Package port declares the interfaces the Billing use cases depend on
// (LLD §3). Adapters implement them; cmd/ wires them together.
package port

// Logger is the structured logger every layer receives. It has the same
// method set as platform-gincommon's logger, so the gincommon logger
// satisfies it directly and app code never imports gincommon.
type Logger interface {
	Debug(msg string, fields map[string]any)
	Info(msg string, fields map[string]any)
	Warn(msg string, fields map[string]any)
	Error(msg string, fields map[string]any)
}
