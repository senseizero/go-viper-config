// Package grpcclients provides a gRPC dial helper that tries a list of URLs
// in order and returns a connection to the first one whose grpc_health_v1
// Check succeeds. The URL-list convention pairs naturally with the parent
// package's CSV-to-slice handling for []string fields, which is
// what makes ephemeral PR environments easy: a PR env sets
// APP_SUKAUTO_URLS="pr-123-sukauto:9000,develop-sukauto:9000" and falls
// back to develop whenever the PR copy is not deployed.
package grpcclients

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer/leastrequest"
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
	// choiceCount > 0 selects least_request over the default round_robin.
	choiceCount uint32
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

// WithLeastRequest switches a balanced conn from the default round_robin to
// least_request: instead of handing every backend an equal NUMBER of RPCs, each
// pick samples choiceCount backends at random and routes to whichever has the
// fewest OUTSTANDING RPCs.
//
// Use it when the backends are NOT equally fast. round_robin is count-fair, not
// load-fair, so a pool mixing a fast and a slow worker over-dispatches the slow
// one: it keeps receiving its half of the RPCs however long its queue grows,
// and the excess eventually times out. least_request instead lets a slow backend
// fill up and then routes around it, so each backend absorbs work in proportion
// to the rate it actually drains it.
//
// This counts long-lived streams, not just unary calls: gRPC increments the
// counter when the picker fires (once, at stream creation) and only decrements
// it from the RPC's Done callback, which for a stream runs at termination. A
// backend chewing on a slow stream therefore stays "loaded" for that stream's
// whole life — which is exactly what makes this the right policy for a pool of
// streaming workers.
//
// choiceCount is gRFC A48's "choice_count": 0 uses gRPC's default of 2 (the
// classic power-of-two-choices). Note the sample is taken WITH replacement, so
// on a 2-backend pool choiceCount=2 draws the same backend twice half the time
// and the routing is only biased, not strictly least-loaded. Raise it for small
// pools — at the maximum of 10 a 2-backend pool misses a backend ~0.2% of the
// time, i.e. it picks the least-loaded one essentially always. Values are
// clamped to A48's [2,10] range, so an out-of-range knob can't fail the dial.
//
// Health checking is unaffected: least_request drives the same client-side
// health listener as round_robin, so NOT_SERVING backends still leave the picker.
func WithLeastRequest(choiceCount uint32) Option {
	return func(o *options) {
		if choiceCount == 0 {
			choiceCount = defaultChoiceCount
		}
		o.choiceCount = min(max(choiceCount, defaultChoiceCount), maxChoiceCount)
	}
}

// gRFC A48 defaults choice_count to 2, rejects anything below it, and caps it
// at 10. Mirrored here so a caller's knob is clamped rather than rejected: an
// invalid default service config makes grpc.NewClient fail, and callers dial at
// boot, so that would be a crashloop rather than a misconfiguration.
const (
	defaultChoiceCount = 2
	maxChoiceCount     = 10
)

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
//
// The policy is round_robin unless the caller asked for WithLeastRequest, so
// every existing caller keeps the exact config it had.
func balancedServiceConfig(choiceCount uint32) string {
	policy := `{"round_robin": {}}`
	if choiceCount > 0 {
		// leastrequest.Name is "least_request_experimental", not "least_request":
		// the policy is still experimental in grpc-go, and the service config must
		// name it exactly as registered or grpc.NewClient rejects the config.
		// Referencing the const (rather than hardcoding the string) also keeps the
		// import non-blank, so the init() that registers the balancer cannot be
		// dropped by a stray goimports run.
		policy = fmt.Sprintf(`{%q: {"choiceCount": %d}}`, leastrequest.Name, choiceCount)
	}
	return fmt.Sprintf(`{
  "loadBalancingConfig": [%s],
  "healthCheckConfig": {"serviceName": ""}
}`, policy)
}

// balancedSchemes numbers the per-conn resolver schemes so that two balanced
// conns in one process (say, a GPU tier and a CPU tier) never collide.
var balancedSchemes atomic.Uint64

// DialBalanced returns a single ClientConn that spreads RPCs across every URL,
// with client-side health checking keeping unhealthy backends out of the picker.
// Backends that recover are picked up again without a redial.
//
// The default policy is round_robin, which is count-fair. When the backends are
// not equally fast, pass WithLeastRequest to route by outstanding load instead.
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
// A single URL carrying a resolver scheme (anything with "://", e.g.
// "dns:///sukyana-headless:6197") is passed to grpc.NewClient as-is, so that
// resolver supplies the addresses. That is how you spread across the pods of a
// Kubernetes Service: a plain ClusterIP is one VIP, so gRPC pins a single pod
// for the life of the connection — point this at a HEADLESS service instead and
// the dns resolver hands back every pod IP.
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

	dialOpts := []grpc.DialOption{grpc.WithDefaultServiceConfig(balancedServiceConfig(o.choiceCount))}
	if o.waitForReady {
		dialOpts = append(dialOpts, grpc.WithDefaultCallOptions(grpc.WaitForReady(true)))
	}

	target := ""
	if len(urls) == 1 && strings.Contains(urls[0], "://") {
		target = urls[0]
	} else {
		addresses := make([]resolver.Address, 0, len(urls))
		for _, url := range urls {
			addresses = append(addresses, resolver.Address{Addr: url})
		}
		// The builder is handed to this ClientConn only (WithResolvers), never
		// to the global registry, so the scheme just has to be unique in-process.
		scheme := fmt.Sprintf("balanced-%d", balancedSchemes.Add(1))
		res := manual.NewBuilderWithScheme(scheme)
		res.InitialState(resolver.State{Addresses: addresses})
		dialOpts = append(dialOpts, grpc.WithResolvers(res))
		target = scheme + ":///backends"
	}
	dialOpts = append(dialOpts, o.dialOpts...)

	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpcclients: balanced dial for %q: %w", o.serviceName, err)
	}
	policy := "round_robin"
	if o.choiceCount > 0 {
		policy = fmt.Sprintf("%s(choiceCount=%d)", leastrequest.Name, o.choiceCount)
	}
	o.logger.Info("grpcclients: balanced conn created",
		"service", o.serviceName, "target", target, "backends", len(urls),
		"waitForReady", o.waitForReady, "policy", policy)
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
