package worker_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/worker"
	"github.com/puppet-stagehand/stagehand-sdk/worker/workertest"
)

// fakeDocuments records the Put calls the worker makes over the facet
// connection.
type fakeDocuments struct {
	hostv1.UnimplementedDocumentsServer
	mu   sync.Mutex
	puts []string
}

func (f *fakeDocuments) Put(_ context.Context, r *hostv1.PutDocumentRequest) (*hostv1.PutDocumentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, r.GetCollection()+"/"+r.GetDocId())
	return &hostv1.PutDocumentResponse{Version: 1}, nil
}

func (f *fakeDocuments) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.puts...)
}

func registerDocuments(f *fakeDocuments) workertest.FacetRegistration {
	return func(s grpc.ServiceRegistrar) { hostv1.RegisterDocumentsServer(s, f) }
}

// start applies the fake host's environment, runs the worker and registers a
// cleanup that stops it. The returned channel yields Run's result.
func start(t *testing.T, host *workertest.FakeHost, opts worker.Options) (context.CancelFunc, <-chan error) {
	t.Helper()
	for k, v := range host.Env() {
		t.Setenv(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("worker.Run did not return after cancel")
		}
	})
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := host.WaitConnected(waitCtx); err != nil {
		t.Fatalf("worker never connected: %v", err)
	}
	return cancel, done
}

const uiManifest = `{"format":1,"entry":"index.js","slots":{},"files":{"index.js":{"sha256":"` +
	"0000000000000000000000000000000000000000000000000000000000000000" +
	`","size":18,"content_type":"text/javascript"}}}`

func testUI() fstest.MapFS {
	return fstest.MapFS{
		"ui.manifest.json": {Data: []byte(uiManifest)},
		"index.js":         {Data: []byte("export default {};\n")},
	}
}

func TestRunConnects(t *testing.T) {
	docs := &fakeDocuments{}
	host := workertest.NewFakeHost(t, registerDocuments(docs))

	connected := make(chan error, 1)
	cancel, done := start(t, host, worker.Options{
		OnConnected: func(ctx context.Context, c *worker.Clients) error {
			body, err := structpb.NewStruct(map[string]any{"greeting": "Hello"})
			if err == nil {
				_, err = c.Documents.Put(ctx, &hostv1.PutDocumentRequest{
					Collection: "greetings", DocId: "en", Body: &hostv1.Json{Value: body},
				})
			}
			connected <- err
			return err
		},
	})
	defer cancel()

	select {
	case err := <-connected:
		if err != nil {
			t.Fatalf("OnConnected facet call: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnConnected was not called")
	}
	if got := docs.seen(); len(got) != 1 || got[0] != "greetings/en" {
		t.Fatalf("host saw puts %v, want [greetings/en]", got)
	}

	// A host Shutdown makes Run return nil.
	if _, err := host.Lifecycle().Shutdown(context.Background(), &emptypb.Empty{}); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after Shutdown = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Shutdown")
	}
}

func TestHealth(t *testing.T) {
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{})

	st, err := host.Health().Check(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !st.GetLive() || !st.GetReady() {
		t.Fatalf("health = live %v ready %v, want both true", st.GetLive(), st.GetReady())
	}
}

func TestAssetsGet(t *testing.T) {
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{UI: testUI()})

	stream, err := host.Assets().Get(context.Background(), &hostv1.AssetRequest{Path: "index.js"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var body []byte
	var ct string
	for {
		c, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		body = append(body, c.GetData()...)
		ct = c.GetContentType()
	}
	if string(body) != "export default {};\n" {
		t.Fatalf("body = %q", body)
	}
	if ct != "text/javascript" {
		t.Fatalf("content type = %q, want text/javascript", ct)
	}
}

func TestRouteDispatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello, " + r.URL.Query().Get("who")))
	})
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{Routes: mux})

	resp, err := host.Routes().Dispatch(context.Background(), &hostv1.HttpRequest{
		Method: "GET", Path: "hello", Query: map[string]string{"who": "world"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if resp.GetStatus() != 200 || string(resp.GetBody()) != "hello, world" {
		t.Fatalf("response = %d %q, want 200 \"hello, world\"", resp.GetStatus(), resp.GetBody())
	}
	if resp.GetHeaders()["Content-Type"] != "text/plain" {
		t.Fatalf("headers = %v", resp.GetHeaders())
	}

	missing, err := host.Routes().Dispatch(context.Background(), &hostv1.HttpRequest{Method: "GET", Path: "nope"})
	if err != nil {
		t.Fatalf("Dispatch missing: %v", err)
	}
	if missing.GetStatus() != 404 {
		t.Fatalf("unrouted status = %d, want 404", missing.GetStatus())
	}
}

func TestMissingEnv(t *testing.T) {
	host := workertest.NewFakeHost(t)
	env := host.Env()
	for k, v := range env {
		t.Setenv(k, v)
	}
	t.Setenv(worker.EnvClientKey, "")

	err := worker.Run(context.Background(), worker.Options{})
	if err == nil {
		t.Fatal("Run succeeded with STAGEHAND_CLIENT_KEY unset")
	}
	if !strings.Contains(err.Error(), "STAGEHAND_CLIENT_KEY") {
		t.Fatalf("error %q does not name STAGEHAND_CLIENT_KEY", err)
	}
	for _, name := range []string{worker.EnvHostAddr, worker.EnvClientCert, worker.EnvCACert} {
		if strings.Contains(err.Error(), env[name]) {
			t.Fatalf("error leaks the value of %s", name)
		}
	}
}

func TestRunReturnsWhenHostDropsTheWorker(t *testing.T) {
	host := workertest.NewFakeHost(t)
	_, done := start(t, host, worker.Options{})

	host.DropWorker()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil after the host dropped the worker, want an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the host dropped the worker")
	}
}
