package web

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// InitTelemetry registers the web layer's instruments.
//
// Called after the meter provider is installed, like the other packages that
// carry their own: a counter created against the global no-op provider stays a
// no-op for the life of the process.
//
// reviewProblems counts the interactions with the review and matching pages that
// did not go through.
//
// It exists because a refusal was previously visible only in a log line and a
// span attribute, and a refusal is not a curiosity: every one of them is a
// person who tried to settle a transaction and was told no. A run of them means
// the page is drawn from state that keeps moving underneath it — a sync
// overlapping the answer, a settings change between drawing and pressing — and
// nothing else in the program would show that.
//
// Refusals and failures share one series and are told apart by outcome. A
// refusal is the program working: the answer was made against a page that had
// gone stale, and applying it would have acted on something other than what was
// on screen. A failure is the program not working. Separate series would make
// the ratio, which is the interesting figure, need a join.

func (s *Server) InitTelemetry() {
	c, err := otel.Meter("bankingsync/web").Int64Counter("bankingsync_review_problems_total",
		metric.WithDescription("Review and matching interactions that did not go through, by "+
			"operation and whether the program refused them or failed at them"))
	if err != nil {
		return
	}
	s.reviewProblems = c
}

func (s *Server) countReviewProblem(ctx context.Context, op, outcome string) {
	if s.reviewProblems == nil {
		return
	}
	s.reviewProblems.Add(ctx, 1, metric.WithAttributes(
		attribute.String("op", op),
		attribute.String("outcome", outcome)))
}
