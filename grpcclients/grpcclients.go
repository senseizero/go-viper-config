// Package grpcclients provides a gRPC dial helper that tries a list of URLs
// in order and returns a connection to the first one whose grpc_health_v1
// Check succeeds. The URL-list convention pairs naturally with the parent
// package's CSV-to-slice handling for env vars ending in "urls", which is
// what makes ephemeral PR environments easy: a PR env sets
// APP_SUKAUTO_URLS="pr-123-sukauto:9000,develop-sukauto:9000" and falls
// back to develop whenever the PR copy is not deployed.
package grpcclients

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// Option configures DialWithFallback.
type Option func(*options)

type options struct {
	dialOpts      []grpc.DialOption
	healthTimeout time.Duration
	serviceName   string
	logger        *slog.Logger
}

func defaults() options {
	return options{
		healthTimeout: 5 * time.Second,
		logger:        slog.Default(),
	}
}

// WithDialOptions passes grpc.DialOption values to grpc.NewClient for every
// URL attempted. Credentials, interceptors, and message-size limits go here;
// the package intentionally sets none by default.
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(o *options) { o.dialOpts = append(o.dialOpts, opts...) }
}

// WithHealthTimeout sets the per-URL grpc_health_v1 Check timeout. Default 5s.
func WithHealthTimeout(d time.Duration) Option {
	return func(o *options) { o.healthTimeout = d }
}

// WithServiceName tags log lines so fallbacks across multiple clients are
// easy to trace in aggregated logs.
func WithServiceName(name string) Option {
	return func(o *options) { o.serviceName = name }
}

// WithLogger overrides the logger. Defaults to slog.Default(). Passing nil
// is a no-op.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

// ErrNoURLs is returned when DialWithFallback is called with an empty URL list.
var ErrNoURLs = errors.New("grpcclients: no URLs configured")

// DialWithFallback tries each URL in order. It returns the first ClientConn
// whose grpc_health_v1 Check succeeds within the configured timeout. Failing
// conns are closed before moving to the next URL. When all URLs fail, the
// returned error wraps the last underlying error.
//
// The caller owns the returned conn and must Close it.
func DialWithFallback(ctx context.Context, urls []string, opts ...Option) (*grpc.ClientConn, error) {
	if len(urls) == 0 {
		return nil, ErrNoURLs
	}
	o := defaults()
	for _, opt := range opts {
		opt(&o)
	}

	var lastErr error
	for i, url := range urls {
		o.logger.Info("grpcclients: attempting connection",
			"service", o.serviceName, "url", url, "attempt", i+1, "total", len(urls))

		conn, err := dialAndProbe(ctx, url, o)
		if err == nil {
			o.logger.Info("grpcclients: connected",
				"service", o.serviceName, "url", url)
			return conn, nil
		}
		o.logger.Warn("grpcclients: connection failed",
			"service", o.serviceName, "url", url, "err", err)
		lastErr = err
	}
	return nil, fmt.Errorf("grpcclients: all %d attempts failed for %q: %w",
		len(urls), o.serviceName, lastErr)
}

func dialAndProbe(ctx context.Context, url string, o options) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(url, o.dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, o.healthTimeout)
	defer cancel()

	if _, err := grpc_health_v1.NewHealthClient(conn).Check(
		probeCtx, &grpc_health_v1.HealthCheckRequest{},
	); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("health check: %w", err)
	}
	return conn, nil
}
