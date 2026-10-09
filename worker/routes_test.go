package worker_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/worker"
	"github.com/puppet-stagehand/stagehand-sdk/worker/workertest"
)

func dispatch(t *testing.T, host *workertest.FakeHost, req *hostv1.HttpRequest) *hostv1.HttpResponse {
	t.Helper()
	resp, err := host.Routes().Dispatch(context.Background(), req)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	return resp
}

func TestRouteRequestMapping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /state/{workspace}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Seen-Method", r.Method)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "%s|%s|%s|%s|%s", r.PathValue("workspace"), r.URL.Query().Get("page"), r.Header.Get("Content-Type"), body, r.URL.Path)
	})
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{Routes: mux})

	resp := dispatch(t, host, &hostv1.HttpRequest{
		Method: "POST", Path: "state/prod", Query: map[string]string{"page": "2"},
		Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"a":1}`),
	})
	if resp.GetStatus() != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.GetStatus())
	}
	if got, want := string(resp.GetBody()), `prod|2|application/json|{"a":1}|/state/prod`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if resp.GetHeaders()["X-Seen-Method"] != "POST" {
		t.Fatalf("headers = %v", resp.GetHeaders())
	}
}

func TestPrincipalComesOnlyFromTheProto(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := worker.PrincipalFrom(r.Context())
		if !ok {
			_, _ = w.Write([]byte("none"))
			return
		}
		_, _ = w.Write([]byte(p.GetLabel() + ":" + strings.Join(p.GetScopes(), ",")))
	})
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{Routes: h})

	forged := map[string]string{
		"X-Stagehand-Principal": "admin",
		"X-Principal-Label":     "admin",
		"X-Forwarded-User":      "admin",
		"Authorization":         "Bearer admin",
	}
	resp := dispatch(t, host, &hostv1.HttpRequest{
		Method: "GET", Path: "who", Headers: forged,
		Principal: &hostv1.Principal{TokenId: "t1", Label: "viewer", Scopes: []string{"read"}},
	})
	if got := string(resp.GetBody()); got != "viewer:read" {
		t.Fatalf("principal = %q, want viewer:read (a forged header must not win)", got)
	}

	resp = dispatch(t, host, &hostv1.HttpRequest{Method: "GET", Path: "who", Headers: forged})
	if got := string(resp.GetBody()); got != "none" {
		t.Fatalf("with no proto principal PrincipalFrom reported %q, want none", got)
	}
}

func TestRequestBodyOverTheCapIs413(t *testing.T) {
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "%d", n)
	})
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{Routes: h, MaxBodyBytes: 1024})

	resp := dispatch(t, host, &hostv1.HttpRequest{Method: "POST", Path: "x", Body: make([]byte, 1025)})
	if resp.GetStatus() != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.GetStatus())
	}
	if calls.Load() != 0 {
		t.Fatalf("handler ran %d times for an oversized request", calls.Load())
	}

	resp = dispatch(t, host, &hostv1.HttpRequest{Method: "POST", Path: "x", Body: make([]byte, 1024)})
	if resp.GetStatus() != 200 || string(resp.GetBody()) != "1024" {
		t.Fatalf("a body exactly at the cap = %d %q, want 200 \"1024\"", resp.GetStatus(), resp.GetBody())
	}
}

func TestResponseBodyOverTheCapIs502(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := 100
		if r.URL.Query().Get("big") != "" {
			n = 2000
		}
		_, _ = w.Write([]byte(strings.Repeat("x", n)))
	})
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{Routes: h, MaxBodyBytes: 1024})

	resp := dispatch(t, host, &hostv1.HttpRequest{Method: "GET", Path: "x", Query: map[string]string{"big": "1"}})
	if resp.GetStatus() != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.GetStatus())
	}
	if len(resp.GetBody()) > 200 {
		t.Fatalf("502 body is %d bytes, want a short error", len(resp.GetBody()))
	}

	resp = dispatch(t, host, &hostv1.HttpRequest{Method: "GET", Path: "x"})
	if resp.GetStatus() != 200 || len(resp.GetBody()) != 100 {
		t.Fatalf("a response under the cap = %d (%d bytes), want 200 (100 bytes)", resp.GetStatus(), len(resp.GetBody()))
	}
}

func TestHandlerPanicIs500AndTheWorkerSurvives(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("handler exploded with secret-looking text")
		}
		_, _ = w.Write([]byte("fine"))
	})
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{Routes: h})

	resp := dispatch(t, host, &hostv1.HttpRequest{Method: "GET", Path: "boom"})
	if resp.GetStatus() != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.GetStatus())
	}
	if strings.Contains(string(resp.GetBody()), "secret-looking") {
		t.Fatalf("500 body leaks the panic value: %q", resp.GetBody())
	}
	resp = dispatch(t, host, &hostv1.HttpRequest{Method: "GET", Path: "ok"})
	if resp.GetStatus() != 200 || string(resp.GetBody()) != "fine" {
		t.Fatalf("next dispatch = %d %q, want 200 \"fine\"", resp.GetStatus(), resp.GetBody())
	}
}

func TestMalformedRequestsAreRefusedWithout500(t *testing.T) {
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	host := workertest.NewFakeHost(t)
	start(t, host, worker.Options{Routes: h})

	for name, req := range map[string]*hostv1.HttpRequest{
		"dot dot segment": {Method: "GET", Path: "a/../b"},
		"bad method":      {Method: "GE T", Path: "a"},
		"nul in path":     {Method: "GET", Path: "a\x00b"},
	} {
		resp := dispatch(t, host, req)
		if resp.GetStatus() != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, resp.GetStatus())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("handler ran %d times for malformed requests", calls.Load())
	}
}
