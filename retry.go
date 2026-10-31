package main

import (
	"context"
	"errors"
	"time"
)

// temporary marks a failure worth trying again: a network error, HTTP 429 or a 5xx.
// A store or an API that is briefly down must never turn into a missing day.
type temporary struct{ err error }

func (t temporary) Error() string { return t.err.Error() }
func (t temporary) Unwrap() error { return t.err }

func retryable(status int) bool { return status == 429 || status >= 500 }

// retryDelays are the waits before the second, third and fourth attempts.
var retryDelays = []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second}

func withRetry(ctx context.Context, f func() error) error {
	for i := 0; ; i++ {
		err := f()
		var t temporary
		if err == nil || !errors.As(err, &t) || i >= len(retryDelays) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryDelays[i]):
		}
	}
}
