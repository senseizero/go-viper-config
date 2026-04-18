package grpcclients_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/senseizero/go-viper-config/grpcclients"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// startServer boots an in-process grpc server registered with a health service
// set to the given status. Returns the "host:port" address and a cleanup fn.
// Pass status == 0 to skip registering the health service entirely so Check
// returns Unimplemented.
func startServer(t *testing.T, status grpc_health_v1.HealthCheckResponse_ServingStatus) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	if status != 0 {
		hs := health.NewServer()
		hs.SetServingStatus("", status)
		grpc_health_v1.RegisterHealthServer(srv, hs)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return lis.Addr().String()
}

func insecureDial() grpcclients.Option {
	return grpcclients.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials()))
}

func TestDialWithFallback_EmptyURLs(t *testing.T) {
	_, err := grpcclients.DialWithFallback(context.Background(), nil)
	if !errors.Is(err, grpcclients.ErrNoURLs) {
		t.Fatalf("want ErrNoURLs, got %v", err)
	}
}

func TestDialWithFallback_HappyPath(t *testing.T) {
	addr := startServer(t, grpc_health_v1.HealthCheckResponse_SERVING)
	conn, err := grpcclients.DialWithFallback(context.Background(),
		[]string{addr}, insecureDial())
	if err != nil {
		t.Fatalf("DialWithFallback: %v", err)
	}
	defer conn.Close()
	if conn.Target() != addr {
		t.Fatalf("target mismatch: got %q want %q", conn.Target(), addr)
	}
}

func TestDialWithFallback_FirstUnreachable(t *testing.T) {
	// Grab a port, close it, so dial fails fast at connect-time.
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := lis.Addr().String()
	_ = lis.Close()

	liveAddr := startServer(t, grpc_health_v1.HealthCheckResponse_SERVING)

	conn, err := grpcclients.DialWithFallback(context.Background(),
		[]string{deadAddr, liveAddr},
		insecureDial(),
		grpcclients.WithHealthTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("DialWithFallback: %v", err)
	}
	defer conn.Close()
	if conn.Target() != liveAddr {
		t.Fatalf("expected fallback to %q, got %q", liveAddr, conn.Target())
	}
}

func TestDialWithFallback_FirstHealthUnimplemented(t *testing.T) {
	// Server reachable but without the health service → Check returns Unimplemented.
	noHealth := startServer(t, 0)
	liveAddr := startServer(t, grpc_health_v1.HealthCheckResponse_SERVING)

	conn, err := grpcclients.DialWithFallback(context.Background(),
		[]string{noHealth, liveAddr},
		insecureDial(),
		grpcclients.WithHealthTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("DialWithFallback: %v", err)
	}
	defer conn.Close()
	if conn.Target() != liveAddr {
		t.Fatalf("expected fallback to %q, got %q", liveAddr, conn.Target())
	}
}

func TestDialWithFallback_AllFail(t *testing.T) {
	lis1, _ := net.Listen("tcp", "127.0.0.1:0")
	lis2, _ := net.Listen("tcp", "127.0.0.1:0")
	a, b := lis1.Addr().String(), lis2.Addr().String()
	_ = lis1.Close()
	_ = lis2.Close()

	_, err := grpcclients.DialWithFallback(context.Background(),
		[]string{a, b},
		insecureDial(),
		grpcclients.WithHealthTimeout(500*time.Millisecond),
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
