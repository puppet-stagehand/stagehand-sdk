// Command hello is the SDK's example Expansion Pack worker.
//
// It is the smallest complete Go pack: one route (GET greeting) that reads a
// document through the Documents facet and says who is asking. Copy it, change
// the route and the document, and keep the three habits it shows:
//
//  1. The caller is named only through worker.PrincipalFrom, which reads the
//     principal field the console resolved. Never read a request header such as
//     "X-User" for identity: a browser can send any header it likes.
//  2. State lives in the Documents facet (manifest permission documents:rw),
//     never in files inside the container.
//  3. Nothing here logs or returns facet errors verbatim; the browser sees a
//     fixed message and the real error stays in the worker.
//
// Build it with the Dockerfile next to this file; docs/worker.md and
// docs/image-layout.md in the SDK explain the rest.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/worker"
)

const (
	// The document the greeting is read from. The host namespaces collections
	// per pack, so "greetings" here cannot collide with another pack's.
	greetingCollection = "greetings"
	greetingDocID      = "default"
	defaultGreeting    = "Hello"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := &app{}
	err := worker.Run(ctx, a.options())
	if err != nil {
		// worker.Run's errors never contain certificate, key or host
		// address material, so printing one is safe.
		log.Printf("hello: %v", err)
		os.Exit(1)
	}
}

// app holds the one thing the handler needs from the host connection: the
// Documents client, which only exists once the host has admitted the worker.
type app struct {
	mu   sync.RWMutex
	docs hostv1.DocumentsClient
}

func (a *app) documents() hostv1.DocumentsClient {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.docs
}

// options wires the app into the worker runtime.
func (a *app) options() worker.Options {
	mux := http.NewServeMux()
	// The console hands the route's relative path ("greeting") to the handler
	// with a leading slash added.
	mux.HandleFunc("GET /greeting", a.greeting)

	return worker.Options{
		Routes: mux,
		// /stagehand/ui is where the image layout puts a UI bundle. This
		// example ships none, so the directory is absent and the worker has
		// no screens to serve; docs/worker.md shows how to add one.
		UI: os.DirFS(worker.DefaultUIDir),
		// Ready is asked by the host's health check. The worker is useful
		// only once the Documents client exists.
		Ready: func(context.Context) (bool, string) {
			if a.documents() == nil {
				return false, "waiting for the host connection"
			}
			return true, ""
		},
		OnConnected: a.onConnected,
	}
}

// onConnected runs once, after the host admits the worker. It keeps the
// Documents client and seeds the greeting document if nobody has yet.
func (a *app) onConnected(ctx context.Context, c *worker.Clients) error {
	a.mu.Lock()
	a.docs = c.Documents
	a.mu.Unlock()

	body, err := structpb.NewStruct(map[string]any{"text": defaultGreeting})
	if err != nil {
		return err
	}
	// if_version 0 means create-only: it never overwrites a greeting an
	// operator has already changed.
	_, err = c.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: greetingCollection,
		DocId:      greetingDocID,
		Body:       &hostv1.Json{Value: body},
		IfVersion:  0,
	})
	switch status.Code(err) {
	case codes.OK, codes.AlreadyExists, codes.Aborted, codes.FailedPrecondition:
		// Created, or it was already there. Either is fine.
		return nil
	default:
		return fmt.Errorf("seed greeting: %w", err)
	}
}

// greeting answers GET greeting with {"greeting": "...", "caller": "..."}.
func (a *app) greeting(w http.ResponseWriter, r *http.Request) {
	docs := a.documents()
	if docs == nil {
		reply(w, http.StatusServiceUnavailable, map[string]string{"error": "not ready"})
		return
	}

	// Identity comes only from the console-resolved principal. Note what is
	// NOT here: no request header is ever read to learn who is asking.
	caller := "unknown"
	if p, ok := worker.PrincipalFrom(r.Context()); ok && p.GetLabel() != "" {
		caller = p.GetLabel()
	}

	text, err := readGreeting(r.Context(), docs)
	if err != nil {
		log.Printf("hello: read greeting: %v", err)
		reply(w, http.StatusBadGateway, map[string]string{"error": "could not read the greeting"})
		return
	}
	reply(w, http.StatusOK, map[string]string{"greeting": text, "caller": caller})
}

// readGreeting reads the greeting text, falling back to the default when the
// document does not exist or has no usable text.
func readGreeting(ctx context.Context, docs hostv1.DocumentsClient) (string, error) {
	doc, err := docs.Get(ctx, &hostv1.GetDocumentRequest{
		Collection: greetingCollection,
		DocId:      greetingDocID,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return defaultGreeting, nil
		}
		return "", err
	}
	if text := doc.GetBody().GetValue().GetFields()["text"].GetStringValue(); text != "" {
		return text, nil
	}
	return defaultGreeting, nil
}

// reply writes a small JSON response. Headers set here are single-valued,
// which is what the worker protocol carries faithfully (a multi-valued header
// is joined with ", ", wrong for Set-Cookie, which a pack behind the console
// should not set anyway).
func reply(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write(b); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("hello: write response: %v", err)
	}
}
