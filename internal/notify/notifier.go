package notify

import (
	"context"

	"cancanneed/internal/model"
)

type Notifier interface {
	NotificationCount(model.Report) int
	Notify(context.Context, model.Report, int) error
	NotifyReviewFailure(context.Context, model.ReviewFailureReport) error
}

type Nop struct{}

func (Nop) NotificationCount(model.Report) int              { return 0 }
func (Nop) Notify(context.Context, model.Report, int) error { return nil }
func (Nop) NotifyReviewFailure(context.Context, model.ReviewFailureReport) error {
	return nil
}
