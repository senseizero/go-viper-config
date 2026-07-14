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

const holdMethod = "/grpcclients.test.Hold/Stream"

// holdBackend serves a CLIENT-STREAMING method, like sukyana's
// "rpc OCR (stream OCRChunk) returns (OCRResponse)". Once stuck is set, its
// handler stops returning, so the stream stays open — which is exactly how a
// slow OCR box looks to the balancer, and the whole reason these tests exist:
// round_robin cannot see that, least_request can.
type holdBackend struct {
	addr    string
	stuck   atomic.Bool
	streams atomic.Int64 // streams the picker sent here
	release chan struct{}
}

func startHoldBackend(t *testing.T) *holdBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &holdBackend{addr: lis.Addr().String(), release: make(chan struct{})}
	t.Cleanup(func() { close(b.release) })

	desc := grpc.ServiceDesc{
		ServiceName: "grpcclients.test.Hold",
		HandlerType: (*echoService)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Stream",
			ClientStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error {
				b.streams.Add(1)
				if b.stuck.Load() {
					select {
					case <-b.release:
					case <-stream.Context().Done():
						return stream.Context().Err()
					}
				}
				return stream.SendMsg(&emptypb.Empty{})
			},
		}},
	}

	srv := grpc.NewServer()
	srv.RegisterService(&desc, struct{}{})
	hs := health.NewServer()
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(srv, hs)

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })
	return b
}

// openHold starts a stream (this is where the picker fires — exactly once) and
// drives it to completion on a goroutine, so a stream parked on a stuck backend
// does not stall the caller.
func openHold(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true}, holdMethod)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	go func() {
		_ = stream.SendMsg(&emptypb.Empty{})
		_ = stream.CloseSend()
		_ = stream.RecvMsg(&emptypb.Empty{}) // returns only when the handler does
	}()
}

// warmUp forces both subchannels READY and both endpoints into the picker.
// grpc.NewClient is lazy, so without this the first streams would all pile onto
// whichever backend connected first and the measurement would be meaningless.
func warmUp(t *testing.T, conn *grpc.ClientConn, backends ...*holdBackend) {
	t.Helper()
	ready := func() bool {
		for _, b := range backends {
			if b.streams.Load() == 0 {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !ready() {
		openHold(t, conn) // nobody is stuck yet, so these complete
		time.Sleep(20 * time.Millisecond)
	}
	if !ready() {
		t.Fatal("warm-up: not every backend entered the picker")
	}
	for _, b := range backends {
		b.streams.Store(0)
	}
}

// dispatch opens n streams one at a time, pausing so a completed stream's Done
// callback (which is what decrements the least_request counter) lands before the
// next pick.
func dispatch(t *testing.T, conn *grpc.ClientConn, n int) {
	t.Helper()
	for range n {
		openHold(t, conn)
		time.Sleep(30 * time.Millisecond)
	}
}

const dispatched = 20

// THE load-bearing test. A backend wedged on an open stream must stop receiving
// new ones. This only works if gRPC treats a streaming RPC as outstanding for
// the stream's whole life — it increments the counter when the picker fires and
// decrements it from the RPC's Done callback, which for a stream runs at
// termination, not at creation. If that ever regresses, this goes red.
//
// Once the stuck backend holds one stream and the fast one holds none, a
// choiceCount of 10 picks the fast one ~99.9% of the time, so the stuck backend
// should take exactly one stream and then be routed around.
func TestDialBalanced_LeastRequestRoutesAroundAStuckBackend(t *testing.T) {
	stuck, fast := startHoldBackend(t), startHoldBackend(t)

	conn, err := grpcclients.DialBalanced([]string{stuck.addr, fast.addr},
		grpcclients.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
		grpcclients.WithLeastRequest(10),
		grpcclients.WithWaitForReady(true),
	)
	if err != nil {
		t.Fatalf("DialBalanced: %v", err)
	}
	defer func() { _ = conn.Close() }()

	warmUp(t, conn, stuck, fast)
	stuck.stuck.Store(true)
	dispatch(t, conn, dispatched)

	got, other := stuck.streams.Load(), fast.streams.Load()
	t.Logf("least_request: stuck=%d fast=%d (of %d)", got, other, dispatched)
	if got > 3 {
		t.Errorf("least_request kept dispatching to the wedged backend: stuck=%d fast=%d (of %d)",
			got, other, dispatched)
	}
	if other < dispatched-4 {
		t.Errorf("the idle backend was starved: stuck=%d fast=%d (of %d)", got, other, dispatched)
	}
}

// The control, and the regression this change exists to fix: round_robin is
// count-fair, so it keeps feeding the wedged backend its half of the stream
// regardless of the fact that it is not draining any of it.
func TestDialBalanced_RoundRobinKeepsFeedingAStuckBackend(t *testing.T) {
	stuck, fast := startHoldBackend(t), startHoldBackend(t)

	conn, err := grpcclients.DialBalanced([]string{stuck.addr, fast.addr},
		grpcclients.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
		grpcclients.WithWaitForReady(true),
	) // no WithLeastRequest: the default policy
	if err != nil {
		t.Fatalf("DialBalanced: %v", err)
	}
	defer func() { _ = conn.Close() }()

	warmUp(t, conn, stuck, fast)
	stuck.stuck.Store(true)
	dispatch(t, conn, dispatched)

	got := stuck.streams.Load()
	t.Logf("round_robin: stuck=%d fast=%d (of %d)", got, fast.streams.Load(), dispatched)
	if got < 3 {
		t.Errorf("expected round_robin to keep dispatching to the wedged backend, got %d of %d",
			got, dispatched)
	}
}

// least_request must not cost the health checking that round_robin gets: it
// enables the same client-side health listener, so NOT_SERVING still evicts.
func TestDialBalanced_LeastRequestStillEvictsOnHealth(t *testing.T) {
	sick, well := startBackend(t, true), startBackend(t, true)

	conn, err := grpcclients.DialBalanced([]string{sick.addr, well.addr},
		grpcclients.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
		grpcclients.WithLeastRequest(10),
		grpcclients.WithWaitForReady(true),
	)
	if err != nil {
		t.Fatalf("DialBalanced: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if !pingUntil(t, conn, func() bool { return sick.calls.Load() > 0 && well.calls.Load() > 0 }) {
		t.Fatal("backends never both received traffic")
	}
	sick.health.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		before, wellBefore := sick.calls.Load(), well.calls.Load()
		for range 20 {
			_ = ping(t, conn)
		}
		if sick.calls.Load() == before && well.calls.Load() > wellBefore {
			return // evicted
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("unhealthy backend was never evicted: sick=%d well=%d",
		sick.calls.Load(), well.calls.Load())
}

// An out-of-range choiceCount is clamped, not rejected: an invalid default
// service config fails grpc.NewClient, and callers dial at boot, so a bad knob
// would be a crashloop rather than a misconfiguration.
func TestDialBalanced_ClampsChoiceCount(t *testing.T) {
	b := startBackend(t, true)
	for _, cc := range []uint32{0, 1, 11, 4096} {
		conn, err := grpcclients.DialBalanced([]string{b.addr},
			grpcclients.WithDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials())),
			grpcclients.WithLeastRequest(cc),
			grpcclients.WithWaitForReady(true),
		)
		if err != nil {
			t.Fatalf("choiceCount %d was rejected: %v", cc, err)
		}
		if err := ping(t, conn); err != nil {
			t.Errorf("choiceCount %d: ping: %v", cc, err)
		}
		_ = conn.Close()
	}
}
