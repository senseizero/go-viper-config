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
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	// Registers the client-side health-checking producer that the
	// "healthCheckConfig" service-config knob below relies on. Without this
	// blank import the knob is silently ignored and unhealthy backends stay in
	// the picker.
	_ "google.golang.org/grpc/health"

	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/status"
)

// Option configures DialWithFallback and DialBalanced.
type Option func(*options)

type options struct {
	dialOpts      []grpc.DialOption
	healthTimeout time.Duration
	serviceName   string
	logger        *slog.Logger
	waitForReady  bool
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

// WithWaitForReady makes every RPC on a balanced conn block until a backend
// becomes ready instead of failing fast with Unavailable. Use it when the
// caller would rather queue than give up — and NOT when the caller wants to
// detect "no backend is free" so it can fall back to another tier.
func WithWaitForReady(wait bool) Option {
	return func(o *options) { o.waitForReady = wait }
}

// ErrNoURLs is returned when DialWithFallback is called with an empty URL list.
var ErrNoURLs = errors.New("grpcclients: no URLs configured")

// balancedServiceConfig spreads RPCs over every resolved address and lets gRPC
// evict backends that report anything other than SERVING.
//
// An empty healthCheckConfig serviceName asks each backend about its overall
// health (the "" service), which is the same question DialWithFallback probes.
// A backend that does not implement grpc.health.v1 answers Unimplemented; per
// gRFC A17 gRPC then disables health checking for that subchannel and keeps it
// in the picker, so services predating the health protocol still receive load.
const balancedServiceConfig = `{
  "loadBalancingConfig": [{"round_robin": {}}],
  "healthCheckConfig": {"serviceName": ""}
}`

// balancedSchemes numbers the per-conn resolver schemes so that two balanced
// conns in one process (say, a GPU tier and a CPU tier) never collide.
var balancedSchemes atomic.Uint64

// DialBalanced returns a single ClientConn that round-robins RPCs across every
// URL, with client-side health checking keeping unhealthy backends out of the
// picker. Backends that recover are picked up again without a redial.
//
// This is the counterpart to DialWithFallback: that one binds ONE endpoint at
// startup and never re-selects, which turns a pool of replicas into a pool of
// one. Use DialBalanced when every URL is an equivalent worker; keep
// DialWithFallback when the list is a priority ladder.
//
// Tiering (e.g. GPUs first, CPUs only as overflow) is expressed by holding one
// balanced conn per tier: RPCs on a conn whose backends are all unhealthy fail
// fast with Unavailable, which is the caller's signal to try the next tier.
// That only works with the default fail-fast behaviour, so do not combine a
// tier conn with WithWaitForReady.
//
// grpc.NewClient connects lazily, so this never blocks and never probes: an
// endpoint that is down at boot simply joins the pool when it comes up.
//
// The caller owns the returned conn and must Close it.
func DialBalanced(urls []string, opts ...Option) (*grpc.ClientConn, error) {
	if len(urls) == 0 {
		return nil, ErrNoURLs
	}
	o := defaults()
	for _, opt := range opts {
		opt(&o)
	}

	addresses := make([]resolver.Address, 0, len(urls))
	for _, url := range urls {
		addresses = append(addresses, resolver.Address{Addr: url})
	}

	// The builder is handed to this ClientConn only (WithResolvers), never to
	// the global registry, so the scheme just has to be unique in-process.
	scheme := fmt.Sprintf("balanced-%d", balancedSchemes.Add(1))
	res := manual.NewBuilderWithScheme(scheme)
	res.InitialState(resolver.State{Addresses: addresses})

	dialOpts := []grpc.DialOption{
		grpc.WithResolvers(res),
		grpc.WithDefaultServiceConfig(balancedServiceConfig),
	}
	if o.waitForReady {
		dialOpts = append(dialOpts, grpc.WithDefaultCallOptions(grpc.WaitForReady(true)))
	}
	dialOpts = append(dialOpts, o.dialOpts...)

	conn, err := grpc.NewClient(scheme+":///backends", dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpcclients: balanced dial for %q: %w", o.serviceName, err)
	}
	o.logger.Info("grpcclients: balanced conn created",
		"service", o.serviceName, "backends", len(urls), "waitForReady", o.waitForReady)
	return conn, nil
}

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
		// Unimplemented means the server is reachable but doesn't register
		// the grpc.health.v1.Health service — common on services predating
		// the health protocol. Per gRPC convention this is "alive, no
		// health protocol", not "unhealthy". Accept the connection.
		if status.Code(err) == codes.Unimplemented {
			return conn, nil
		}
		_ = conn.Close()
		return nil, fmt.Errorf("health check: %w", err)
	}
	return conn, nil
}
