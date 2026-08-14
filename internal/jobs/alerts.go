package jobs

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

// AlertEvaluator is one pass of threshold evaluation.
type AlertEvaluator interface {
	Evaluate(ctx context.Context) error
}

// AlertInterval is how often rules are checked. A minute matches the metric
// collection interval: evaluating faster would re-read the same sample, and
// slower would make a ten-minute sustained rule fire late.
const AlertInterval = time.Minute

// RunAlertEvaluator ticks the evaluator until the context ends.
func RunAlertEvaluator(ctx context.Context, evaluator AlertEvaluator, log zerolog.Logger) {
	go func() {
		ticker := time.NewTicker(AlertInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := evaluator.Evaluate(ctx); err != nil {
					// A failed pass is logged and retried on the next tick:
					// alerting that stops silently is the worst outcome here.
					log.Error().Err(err).Str("component", "alerts").
						Msg("alert evaluation failed")
				}
			}
		}
	}()
}
