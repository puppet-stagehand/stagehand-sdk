package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/worker"
	"github.com/puppet-stagehand/stagehand-sdk/worker/workertest"
)

// fakeDocuments is an in-memory Documents facet: create-only Put, Get with
// NotFound, which is all the example uses.
type fakeDocuments struct {
	hostv1.UnimplementedDocumentsServer
	mu   sync.Mutex
	docs map[string]*structpb.Struct
}

func (f *fakeDocuments) Put(_ context.Context, r *hostv1.PutDocumentRequest) (*hostv1.PutDocumentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.GetCollection() + "/" + r.GetDocId()
	if _, exists := f.docs[key]; exists && r.GetIfVersion() == 0 {
		return nil, status.Error(codes.AlreadyExists, "exists")
	}
	f.docs[key] = r.GetBody().GetValue()
	return &hostv1.PutDocumentResponse{Version: 1}, nil
}

func (f *fakeDocuments) Get(_ context.Context, r *hostv1.GetDocumentRequest) (*hostv1.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.docs[r.GetCollection()+"/"+r.GetDocId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such document")
	}
	return &hostv1.Document{Collection: r.GetCollection(), DocId: r.GetDocId(), Body: &hostv1.Json{Value: body}, Version: 1}, nil
}

// run starts the example worker against a fake host and waits until it has been
// admitted and its OnConnected hook has finished (observed through readiness).
func run(t *testing.T, docs *fakeDocuments) *workertest.FakeHost {
	t.Helper()
	host := workertest.NewFakeHost(t, func(s grpc.ServiceRegistrar) { hostv1.RegisterDocumentsServer(s, docs) })
	for k, v := range host.Env() {
		t.Setenv(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		a := &app{}
		_ = worker.Run(ctx, a.options())
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Errorf("worker.Run did not return after cancel")
		}
	})

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := host.WaitConnected(waitCtx); err != nil {
		t.Fatalf("worker never connected: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := host.Health().Check(context.Background(), &emptypb.Empty{})
		if err == nil && st.GetReady() {
			return host
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker never became ready (last err %v)", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type greetingBody struct {
	Greeting string `json:"greeting"`
	Caller   string `json:"caller"`
}

func dispatch(t *testing.T, host *workertest.FakeHost, req *hostv1.HttpRequest) (*hostv1.HttpResponse, greetingBody) {
	t.Helper()
	resp, err := host.Routes().Dispatch(context.Background(), req)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	var b greetingBody
	if resp.GetStatus() == 200 {
		if err := json.Unmarshal(resp.GetBody(), &b); err != nil {
			t.Fatalf("response body %q is not JSON: %v", resp.GetBody(), err)
		}
	}
	return resp, b
}

func TestGreetingReadsTheSeededDocumentAndNamesTheCaller(t *testing.T) {
	docs := &fakeDocuments{docs: map[string]*structpb.Struct{}}
	host := run(t, docs)

	resp, got := dispatch(t, host, &hostv1.HttpRequest{
		Method:    "GET",
		Path:      "greeting",
		Principal: &hostv1.Principal{TokenId: "t1", Label: "ada"},
	})
	if resp.GetStatus() != 200 {
		t.Fatalf("status = %d, body %q", resp.GetStatus(), resp.GetBody())
	}
	if got.Greeting != "Hello" || got.Caller != "ada" {
		t.Fatalf("got %+v, want greeting Hello caller ada", got)
	}
	if ct := resp.GetHeaders()["Content-Type"]; ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
}

func TestGreetingUsesAnOperatorChangedDocumentAndSeedingNeverOverwritesIt(t *testing.T) {
	body, err := structpb.NewStruct(map[string]any{"text": "Howdy"})
	if err != nil {
		t.Fatal(err)
	}
	docs := &fakeDocuments{docs: map[string]*structpb.Struct{"greetings/default": body}}
	host := run(t, docs)

	_, got := dispatch(t, host, &hostv1.HttpRequest{Method: "GET", Path: "greeting", Principal: &hostv1.Principal{Label: "ada"}})
	if got.Greeting != "Howdy" {
		t.Fatalf("greeting = %q, want Howdy (the seed must not overwrite an existing document)", got.Greeting)
	}
}

func TestCallerComesOnlyFromThePrincipalNotFromHeaders(t *testing.T) {
	docs := &fakeDocuments{docs: map[string]*structpb.Struct{}}
	host := run(t, docs)

	forged := map[string]string{
		"X-User": "root", "X-Stagehand-User": "root", "X-Principal": "root", "X-Principal-Label": "root",
	}
	resp, got := dispatch(t, host, &hostv1.HttpRequest{Method: "GET", Path: "greeting", Headers: forged})
	if resp.GetStatus() != 200 {
		t.Fatalf("status = %d", resp.GetStatus())
	}
	if got.Caller != "unknown" {
		t.Fatalf("caller = %q with no principal and forged headers, want unknown", got.Caller)
	}

	_, got = dispatch(t, host, &hostv1.HttpRequest{
		Method: "GET", Path: "greeting", Headers: forged, Principal: &hostv1.Principal{Label: "ada"},
	})
	if got.Caller != "ada" {
		t.Fatalf("caller = %q, want ada (the principal wins over headers)", got.Caller)
	}
}

func TestOtherMethodsAndPathsAreNotRouted(t *testing.T) {
	docs := &fakeDocuments{docs: map[string]*structpb.Struct{}}
	host := run(t, docs)

	for _, req := range []*hostv1.HttpRequest{
		{Method: "POST", Path: "greeting"},
		{Method: "GET", Path: "elsewhere"},
	} {
		resp, _ := dispatch(t, host, req)
		if resp.GetStatus() == 200 {
			t.Fatalf("%s %s answered 200, want a refusal", req.GetMethod(), req.GetPath())
		}
	}
}
