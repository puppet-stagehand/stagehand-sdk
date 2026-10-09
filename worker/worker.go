// Package worker is the Expansion Pack worker runtime (Phase 57, D-08).
//
// A pack author writes the pack's own logic as a plain net/http Handler and
// calls Run. Everything between the pack and the console is here, once, so no
// pack copies transport code: the environment contract, mutual TLS with ALPN
// h2, the multiplexed (yamux) session, the reverse gRPC server for Routes,
// Assets, Health and Lifecycle, and the forward connection the pack uses to
// call the host facets.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// The environment contract: the host starts the worker container with these.
// The three PEM variables carry the PEM CONTENT, never a file path, so a
// worker behaves the same whether the container runtime is local or remote.
const (
	EnvHostAddr   = "STAGEHAND_HOST_ADDR"
	EnvClientCert = "STAGEHAND_CLIENT_CERT"
	EnvClientKey  = "STAGEHAND_CLIENT_KEY"
	EnvCACert     = "STAGEHAND_CA_CERT"
)

const (
	// MaxAssetChunk is the largest Assets chunk the worker sends. The console
	// refuses chunks over 1 MiB; 256 KiB stays well inside that.
	MaxAssetChunk = 256 << 10
	// DefaultMaxBodyBytes caps a route request body and a route response body.
	DefaultMaxBodyBytes = 4 << 20
	// DefaultUIDir is where the image layout puts the pack's UI bundle.
	DefaultUIDir = "/stagehand/ui"
)

// Options configures Run. The zero value is a valid, empty pack: no routes,
// no UI, always ready.
type Options struct {
	// Routes receives the console-proxied HTTP requests, with the route's
	// relative path as the URL path (a leading slash is added). Nil answers
	// every request with 404.
	Routes http.Handler
	// UI is the pack's UI bundle (its root holds ui.manifest.json). Nil means
	// os.DirFS(DefaultUIDir). A missing bundle is a pack without a UI.
	UI fs.FS
	// Ready reports readiness for Health.Check; nil means always ready. The
	// string is shown to operators when not ready.
	Ready func(context.Context) (bool, string)
	// OnShutdown runs when the host asks the worker to stop; Run returns once
	// it has finished. The worker stops even if the hook returns an error.
	OnShutdown func(context.Context) error
	// OnPurge runs when the host asks the pack to clean up before it purges
	// the pack's documents; an error is reported to the host as gRPC Internal.
	//
	// Without it Purge succeeds. A pack that keeps state OUTSIDE host-managed
	// storage (the Documents facet, which the host purges itself) MUST set
	// OnPurge: otherwise the host is told the purge succeeded while that state
	// is still there.
	OnPurge func(context.Context) error
	// MaxBodyBytes caps a route request body and response body. Zero means
	// DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// OnConnected runs once, after the host has admitted the worker and the
	// facet connection is up. An error stops Run.
	OnConnected func(context.Context, *Clients) error
}

// Clients are the host facet clients, all over the one forward connection.
// The host enforces the manifest's permissions on every call; these clients do
// not (and cannot) check them.
type Clients struct {
	Documents hostv1.DocumentsClient
	Settings  hostv1.SettingsClient
	Secrets   hostv1.SecretsClient
	Auth      hostv1.AuthClient
	Inventory hostv1.InventoryClient
	Code      hostv1.CodeClient
	Forge     hostv1.ForgeClient
}

// Run connects to the host and serves until the host asks the worker to shut
// down or ctx is cancelled (both return nil), or until something fails: a
// missing or bad environment, the host refusing the worker, an OnConnected
// error, or the host connection being lost (all return an error).
//
// Run never logs, and no error it returns contains certificate, key or CA
// material or the host address.
func Run(ctx context.Context, opts Options) error {
	env, err := readEnv()
	if err != nil {
		return err
	}
	tlsCfg, err := env.tlsConfig()
	if err != nil {
		return err
	}
	ui := opts.UI
	if ui == nil {
		ui = os.DirFS(DefaultUIDir)
	}
	assets, err := newAssetsServer(ui)
	if err != nil {
		return err
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	dialCtx, dialCancel := context.WithTimeout(runCtx, dialTimeout)
	defer dialCancel()
	session, err := env.dial(dialCtx, tlsCfg)
	if err != nil {
		return err
	}
	defer session.Close()

	// Reverse side first: the host may open streams the moment it has admitted
	// this worker, so Routes, Assets, Health and Lifecycle must already serve.
	srv := grpc.NewServer(grpc.MaxRecvMsgSize(int(maxBody) + recvOverhead))
	hostv1.RegisterRoutesServer(srv, &routesServer{handler: opts.Routes, maxBody: maxBody})
	hostv1.RegisterAssetsServer(srv, assets)
	hostv1.RegisterHealthServer(srv, &healthServer{ready: opts.Ready})
	hostv1.RegisterLifecycleServer(srv, &lifecycleServer{onShutdown: opts.OnShutdown, onPurge: opts.OnPurge, stop: stop})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(&sessionListener{session: session}) }()
	defer stopServer(srv)

	// Forward side: the first stream the worker opens carries the facet calls
	// and completes admission on the host.
	cc, err := openForward(session)
	if err != nil {
		return err
	}
	defer cc.Close()
	if err := waitReady(dialCtx, cc); err != nil {
		return env.redact(err)
	}
	dialCancel()

	if opts.OnConnected != nil {
		if err := opts.OnConnected(runCtx, newClients(cc)); err != nil {
			return fmt.Errorf("worker: OnConnected: %w", err)
		}
	}

	select {
	case <-runCtx.Done():
		return nil
	case <-session.CloseChan():
		return errors.New("worker: the host closed the connection")
	case <-served:
		return errors.New("worker: the reverse server stopped")
	}
}

// recvOverhead is headroom over MaxBodyBytes for the rest of an HttpRequest
// message (method, path, query, headers, principal), so an oversized body is
// answered with 413 by Routes rather than refused by gRPC.
const recvOverhead = 1 << 20

// stopServer stops the reverse server, giving in-flight calls (including the
// Shutdown call that is stopping the worker) a moment to finish.
func stopServer(srv *grpc.Server) {
	done := make(chan struct{})
	go func() { srv.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		srv.Stop()
		<-done
	}
}
