package worker_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/worker"
	"github.com/puppet-stagehand/stagehand-sdk/worker/workertest"
)

// mdRecorder is a fake host's view of the stagehand-request-id metadata on
// each facet call, in arrival order. A call that carried none records "".
type mdRecorder struct {
	mu   sync.Mutex
	seen []string
	// raw keeps the full value list so a test can prove one value, not two.
	raw [][]string
}

func (r *mdRecorder) record(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(worker.RequestIDMetadataKey)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.raw = append(r.raw, vals)
	if len(vals) == 0 {
		r.seen = append(r.seen, "")
		return
	}
	r.seen = append(r.seen, vals[0])
}

func (r *mdRecorder) all() ([]string, [][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...), append([][]string(nil), r.raw...)
}

// ridDocuments records request-id metadata on Documents.Put.
type ridDocuments struct {
	hostv1.UnimplementedDocumentsServer
	rec *mdRecorder
}

func (d *ridDocuments) Put(ctx context.Context, _ *hostv1.PutDocumentRequest) (*hostv1.PutDocumentResponse, error) {
	d.rec.record(ctx)
	return &hostv1.PutDocumentResponse{Version: 1}, nil
}

// ridSettings records request-id metadata on the Settings.Subscribe server
// stream and sends one change.
type ridSettings struct {
	hostv1.UnimplementedSettingsServer
	rec *mdRecorder
}

func (s *ridSettings) Subscribe(_ *emptypb.Empty, stream grpc.ServerStreamingServer[hostv1.SettingsChange]) error {
	s.rec.record(stream.Context())
	return stream.Send(&hostv1.SettingsChange{Version: 1, At: timestamppb.Now()})
}

// ridHost starts a fake host with recording Documents and Settings servers
// and a worker whose route handler runs fn with the request and the facet
// clients. It returns the recorder and a dispatch func.
func ridHost(t *testing.T, onConnected func(context.Context, *worker.Clients) error, fn func(w http.ResponseWriter, r *http.Request, c *worker.Clients)) (*mdRecorder, func(id string) *hostv1.HttpResponse) {
	t.Helper()
	rec := &mdRecorder{}
	host := workertest.NewFakeHost(t,
		func(s grpc.ServiceRegistrar) { hostv1.RegisterDocumentsServer(s, &ridDocuments{rec: rec}) },
		func(s grpc.ServiceRegistrar) { hostv1.RegisterSettingsServer(s, &ridSettings{rec: rec}) },
	)
	clients := make(chan *worker.Clients, 1)
	start(t, host, worker.Options{
		OnConnected: func(ctx context.Context, c *worker.Clients) error {
			clients <- c
			if onConnected != nil {
				return onConnected(ctx, c)
			}
			return nil
		},
		Routes: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := <-clients
			clients <- c
			fn(w, r, c)
		}),
	})
	dispatch := func(id string) *hostv1.HttpResponse {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := host.Routes().Dispatch(ctx, &hostv1.HttpRequest{Method: "POST", Path: "deploys", RequestId: id})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if resp.GetStatus() != http.StatusOK {
			t.Fatalf("Dispatch status %d: %s", resp.GetStatus(), resp.GetBody())
		}
		return resp
	}
	return rec, dispatch
}

func putDoc(ctx context.Context, c *worker.Clients) error {
	_, err := c.Documents.Put(ctx, &hostv1.PutDocumentRequest{Collection: "deploy-history", DocId: "d1", Body: &hostv1.Json{}})
	return err
}

func subscribeOnce(ctx context.Context, c *worker.Clients) error {
	stream, err := c.Settings.Subscribe(ctx, &emptypb.Empty{})
	if err != nil {
		return err
	}
	_, err = stream.Recv()
	return err
}

// TestRequestIDReachesFacetCalls: the host's request_id reaches the handler
// through RequestIDFrom and travels as stagehand-request-id metadata on both a
// unary and a server-streaming facet call made with the request ctx (D-32).
func TestRequestIDReachesFacetCalls(t *testing.T) {
	var handlerSaw string
	rec, dispatch := ridHost(t, nil, func(w http.ResponseWriter, r *http.Request, c *worker.Clients) {
		handlerSaw = worker.RequestIDFrom(r.Context())
		if err := putDoc(r.Context(), c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// A pack that also sets its own outgoing metadata keeps it, and the
		// request id is still sent exactly once.
		ctx := metadata.AppendToOutgoingContext(r.Context(), "stagehand-approver-token", "tok")
		if err := subscribeOnce(ctx, c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	dispatch("rid-1")

	if handlerSaw != "rid-1" {
		t.Fatalf("RequestIDFrom(route ctx) = %q, want rid-1", handlerSaw)
	}
	seen, raw := rec.all()
	if len(seen) != 2 || seen[0] != "rid-1" || seen[1] != "rid-1" {
		t.Fatalf("host saw request ids %q, want [rid-1 rid-1] (unary Put, then Subscribe stream)", seen)
	}
	for i, vals := range raw {
		if len(vals) != 1 {
			t.Fatalf("call %d carried %d %s values %q, want exactly one", i, len(vals), worker.RequestIDMetadataKey, vals)
		}
	}
}

// TestNoRequestIDWithoutRequest: a facet call from OnConnected's ctx or from
// context.Background() carries no request id; the worker never invents one
// (SDK-04, T-57.6-04).
func TestNoRequestIDWithoutRequest(t *testing.T) {
	connected := make(chan error, 1)
	rec, _ := ridHost(t, func(ctx context.Context, c *worker.Clients) error {
		if got := worker.RequestIDFrom(ctx); got != "" {
			connected <- errString("RequestIDFrom(OnConnected ctx) = " + got)
			return nil
		}
		err := putDoc(ctx, c)
		if err == nil {
			err = subscribeOnce(ctx, c)
		}
		if err == nil {
			err = putDoc(context.Background(), c)
		}
		connected <- err
		return nil
	}, func(http.ResponseWriter, *http.Request, *worker.Clients) {})

	select {
	case err := <-connected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnConnected was not called")
	}
	seen, _ := rec.all()
	if len(seen) != 3 {
		t.Fatalf("host saw %d facet calls, want 3", len(seen))
	}
	for i, id := range seen {
		if id != "" {
			t.Fatalf("call %d carried request id %q, want none", i, id)
		}
	}
	if got := worker.RequestIDFrom(context.Background()); got != "" {
		t.Fatalf("RequestIDFrom(Background) = %q, want empty", got)
	}
}

// TestRequestIDSurvivesDerivedContext: a ctx derived from the request ctx
// keeps the id; a fresh context.Background() in a goroutine does not.
func TestRequestIDSurvivesDerivedContext(t *testing.T) {
	var derivedSaw string
	rec, dispatch := ridHost(t, nil, func(w http.ResponseWriter, r *http.Request, c *worker.Clients) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		derivedSaw = worker.RequestIDFrom(ctx)
		if err := putDoc(ctx, c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		done := make(chan error, 1)
		go func() { done <- putDoc(context.Background(), c) }()
		if err := <-done; err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	dispatch("rid-derived")

	if derivedSaw != "rid-derived" {
		t.Fatalf("RequestIDFrom(derived ctx) = %q, want rid-derived", derivedSaw)
	}
	seen, _ := rec.all()
	if len(seen) != 2 || seen[0] != "rid-derived" || seen[1] != "" {
		t.Fatalf("host saw %q, want [rid-derived \"\"] (derived ctx, then a fresh Background)", seen)
	}
}

// TestRequestIDBounded: an incoming request_id over 128 bytes or with a
// character outside [A-Za-z0-9._-] is dropped, so a malformed host value never
// reaches the wire or the handler (SDK-03, T-57.6-03). Exactly 128 bytes of
// allowed characters is kept.
func TestRequestIDBounded(t *testing.T) {
	var handlerSaw []string
	var mu sync.Mutex
	rec, dispatch := ridHost(t, nil, func(w http.ResponseWriter, r *http.Request, c *worker.Clients) {
		mu.Lock()
		handlerSaw = append(handlerSaw, worker.RequestIDFrom(r.Context()))
		mu.Unlock()
		if err := putDoc(r.Context(), c); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	max := strings.Repeat("a", 128)
	cases := []struct{ in, want string }{
		{max, max},
		{max + "a", ""},
		{"rid 1", ""},
		{"rid/1", ""},
		{"rid\n1", ""},
		{"rid:1", ""},
		{"réd", ""},
		{"", ""},
		{"A-z_0.9", "A-z_0.9"},
	}
	for _, c := range cases {
		dispatch(c.in)
	}
	seen, _ := rec.all()
	if len(seen) != len(cases) || len(handlerSaw) != len(cases) {
		t.Fatalf("host saw %d calls, handler ran %d times, want %d each", len(seen), len(handlerSaw), len(cases))
	}
	for i, c := range cases {
		if seen[i] != c.want {
			t.Errorf("request_id %q: wire carried %q, want %q", c.in, seen[i], c.want)
		}
		if handlerSaw[i] != c.want {
			t.Errorf("request_id %q: RequestIDFrom = %q, want %q", c.in, handlerSaw[i], c.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
