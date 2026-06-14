package api

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

func retryWithBackoff[T any](ctx context.Context, cfg retryConfig, operation func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		result, err := operation()
		if err == nil {
			return result, nil
		}
		if attempt == cfg.MaxRetries || !isRetryable(err) {
			return zero, err
		}
		delay := time.Duration(1<<attempt) * cfg.BaseDelay
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}
	return zero, fmt.Errorf("max retries exceeded")
}

func retryConnectHTTP(
	ctx context.Context,
	cfg retryConfig,
	connect func(context.Context) (*http.Response, error),
	shouldRetryStatus func(statusCode int) bool,
) (*http.Response, error) {
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		resp, err := connect(ctx)
		if err != nil {
			if attempt == cfg.MaxRetries {
				return nil, err
			}
			delay := time.Duration(1<<attempt) * cfg.BaseDelay
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}

		if shouldRetryStatus != nil && shouldRetryStatus(resp.StatusCode) {
			if attempt == cfg.MaxRetries {
				return resp, nil
			}
			resp.Body.Close()
			delay := time.Duration(1<<attempt) * cfg.BaseDelay
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}

		return resp, nil
	}

	return nil, fmt.Errorf("max retries exceeded")
}
