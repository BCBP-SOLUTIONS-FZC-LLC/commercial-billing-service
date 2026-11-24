package main

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/app/port"
)

// pollLoop runs once every interval until ctx ends; a full batch polls
// again at once.
func pollLoop(ctx context.Context, interval time.Duration, batch int, once func(ctx context.Context, limit int) (int, error), log port.Logger, name string) {
	for {
		n, err := once(ctx, batch)
		if err != nil && ctx.Err() == nil {
			log.Warn(name+" pass failed", map[string]any{"error": err.Error()})
		}
		if err == nil && n >= batch {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
