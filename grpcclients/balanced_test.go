package grpcclients_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/senseizero/go-viper-config/grpcclients"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/emptypb"
)

const pingMethod = "/grpcclients.test.Echo/Ping"

type echoService any

// pingDesc is a hand-rolled service so the tests can count RPCs per backend
// without pulling in protoc. Health/Check would not do: an RPC to a backend
// that never registered the health service is rejected before any interceptor
// runs, and one of the tests is precisely about such a backend still receiving
// traffic.
var pingDesc = grpc.ServiceDesc{
	ServiceName: "grpcclients.test.Echo",
	HandlerType: (*echoService)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Ping",
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(emptypb.Empty)
			if err := dec(in); err != nil {
				return nil, err
			}
			handler := func(context.Context, any) (any, error) { return &emptypb.Empty{}, nil }
			if interceptor == nil {
				return handler(ctx, in)
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: pingMethod}, handler)
		},
	}},
}

type backend struct {
	addr   string
	health *health.Server // nil when the backend predates the health protocol
	calls  atomic.Int64
}

// startBackend boots an in-process server exposing Ping. withHealth=false skips
// the health service entirely, so Check/Watch answer Unimplemented.
func startBackend(t *testing.T, withHealth bool) *backend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &backend{addr: lis.Addr().String()}

	srv := grpc.NewServer(grpc.UnaryInterceptor(
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			b.calls.Add(1)
			return handler(ctx, req)
		}))
	srv.RegisterService(&pingDesc, struct{}{})
	if withHealth {
		b.health = health.NewServer()
		b.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
		grpc_health_v1.RegisterHealthServer(srv, b.health)
	}

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })
	return b
}

func dialBalanced(t *testing.T, backends ...*backend) *grpc.ClientConn {
	t.Helper()
	urls := make([]string, len(backends))
	for i, b := range backends {
		urls[i] = b.addr
	}
	conn, err := grpcclients.DialBalanced(urls,
		grpcclients.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
		grpcclients.WithServiceName("test"),
		grpcclients.WithWaitForReady(true),
	)
	if err != nil {
		t.Fatalf("DialBalanced: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func ping(t *testing.T, conn *grpc.ClientConn) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return conn.Invoke(ctx, pingMethod, &emptypb.Empty{}, &emptypb.Empty{})
}

// pingUntil keeps pinging until cond holds or the deadline passes. Subchannels
// connect lazily, so a fixed number of RPCs would be racy.
func pingUntil(t *testing.T, conn *grpc.ClientConn, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_ = ping(t, conn)
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// The whole point: one conn, every backend gets work. DialWithFallback would
// have pinned the first one.
func TestDialBalanced_SpreadsAcrossEveryBackend(t *testing.T) {
	a, b, c := startBackend(t, true), startBackend(t, true), startBackend(t, true)
	conn := dialBalanced(t, a, b, c)

	all := func() bool { return a.calls.Load() > 0 && b.calls.Load() > 0 && c.calls.Load() > 0 }
	if !pingUntil(t, conn, all) {
		t.Fatalf("round_robin did not reach every backend: a=%d b=%d c=%d",
			a.calls.Load(), b.calls.Load(), c.calls.Load())
	}
}

// A backend that reports NOT_SERVING must leave the picker, and come back when
// it recovers — without redialling.
func TestDialBalanced_EvictsAndRestoresOnHealth(t *testing.T) {
	sick, well := startBackend(t, true), startBackend(t, true)
	conn := dialBalanced(t, sick, well)

	if !pingUntil(t, conn, func() bool { return sick.calls.Load() > 0 && well.calls.Load() > 0 }) {
		t.Fatal("backends never both received traffic")
	}

	sick.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	// Eviction is not instantaneous: an RPC already picked can still land while
	// the health watch propagates. So don't assert "no more calls ever" — assert
	// that a whole burst eventually lands entirely on the healthy backend.
	frozen := int64(-1)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		before, wellBefore := sick.calls.Load(), well.calls.Load()
		for range 20 {
			_ = ping(t, conn)
		}
		if sick.calls.Load() == before && well.calls.Load() > wellBefore {
			frozen = before
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if frozen < 0 {
		t.Fatalf("unhealthy backend was never evicted: sick=%d well=%d", sick.calls.Load(), well.calls.Load())
	}

	sick.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	if !pingUntil(t, conn, func() bool { return sick.calls.Load() > frozen }) {
		t.Fatal("recovered backend never rejoined the picker")
	}
}

// gRFC A17: a backend with no health service answers Unimplemented, and gRPC
// then treats it as healthy rather than dropping it. Our fleet still has
// services predating the health protocol, so this must keep working.
func TestDialBalanced_KeepsBackendsWithoutHealthService(t *testing.T) {
	legacy, modern := startBackend(t, false), startBackend(t, true)
	conn := dialBalanced(t, legacy, modern)

	if !pingUntil(t, conn, func() bool { return legacy.calls.Load() > 0 && modern.calls.Load() > 0 }) {
		t.Fatalf("a backend without the health service was dropped: legacy=%d modern=%d",
			legacy.calls.Load(), modern.calls.Load())
	}
}

// Tiering depends on this: when no backend is usable, a fail-fast RPC must
// return promptly so the caller can try the next tier instead of hanging.
func TestDialBalanced_FailsFastWhenEveryBackendIsUnhealthy(t *testing.T) {
	a, b := startBackend(t, true), startBackend(t, true)
	urls := []string{a.addr, b.addr}
	conn, err := grpcclients.DialBalanced(urls,
		grpcclients.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
	) // no WithWaitForReady: this conn is a tier
	if err != nil {
		t.Fatalf("DialBalanced: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if !pingUntil(t, conn, func() bool { return a.calls.Load()+b.calls.Load() > 0 }) {
		t.Fatal("no backend ever served")
	}
	a.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	b.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		start := time.Now()
		err := ping(t, conn)
		if err != nil {
			if time.Since(start) > 2*time.Second {
				t.Fatalf("RPC blocked for %s instead of failing fast", time.Since(start))
			}
			return // failed fast, as a tier needs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("RPCs kept succeeding after every backend went NOT_SERVING")
}

func TestDialBalanced_NoURLs(t *testing.T) {
	if _, err := grpcclients.DialBalanced(nil); err == nil {
		t.Fatal("expected an error with no URLs")
	}
}

// A single URL carrying a resolver scheme is handed to grpc.NewClient verbatim,
// so the named resolver supplies the addresses. In the cluster that means
// "dns:///headless-svc:port" — a plain ClusterIP would pin one pod. Here
// "passthrough:///" stands in for it, since dns:/// would need real DNS.
func TestDialBalanced_PassesThroughASchemedTarget(t *testing.T) {
	b := startBackend(t, true)

	conn, err := grpcclients.DialBalanced([]string{"passthrough:///" + b.addr},
		grpcclients.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
		grpcclients.WithWaitForReady(true),
	)
	if err != nil {
		t.Fatalf("DialBalanced: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if !pingUntil(t, conn, func() bool { return b.calls.Load() > 0 }) {
		t.Fatal("a schemed target never reached its backend")
	}
}
