// Package workertest is an in-process fake of the console's side of the
// worker protocol, for testing code built on the worker package.
//
// It speaks the same sequence the console's registrar does: an mTLS listener
// that requires a client certificate and negotiates ALPN h2, a yamux server
// session on the accepted connection, the worker's first stream taken as the
// facet connection (served by gRPC servers the test registers), and a gRPC
// client connection back to the worker over a stream the host opens, for
// Routes, Assets, Health and Lifecycle.
//
// All key material is generated fresh for each FakeHost; nothing here is a
// real credential.
package workertest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// FacetRegistration registers a fake facet service (for example a
// hostv1.DocumentsServer) on the server that answers the worker's facet calls.
type FacetRegistration func(grpc.ServiceRegistrar)

// FakeHost is the console's side of the protocol.
type FakeHost struct {
	t         testing.TB
	ln        net.Listener
	srvTLS    *tls.Config
	env       map[string]string
	facets    []FacetRegistration
	timeout   time.Duration
	connected chan struct{}

	mu       sync.Mutex
	session  *yamux.Session
	rev      *grpc.ClientConn
	facetSrv *grpc.Server
	closed   atomic.Bool
	err      error
}

// NewFakeHost starts a fake host listening on a loopback port. Cleanup is
// registered on t.
func NewFakeHost(t testing.TB, facets ...FacetRegistration) *FakeHost {
	t.Helper()
	ca, caKey := newCA(t)
	serverCert := newLeaf(t, ca, caKey, "stagehand-fake-host", true)
	clientCert := newLeaf(t, ca, caKey, "stagehand-fake-worker", false)

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	srvTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert.tls},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2"},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("workertest: listen: %v", err)
	}
	h := &FakeHost{
		t:         t,
		ln:        ln,
		srvTLS:    srvTLS,
		facets:    facets,
		timeout:   10 * time.Second,
		connected: make(chan struct{}),
		env: map[string]string{
			"STAGEHAND_HOST_ADDR":   ln.Addr().String(),
			"STAGEHAND_CLIENT_CERT": clientCert.certPEM,
			"STAGEHAND_CLIENT_KEY":  clientCert.keyPEM,
			"STAGEHAND_CA_CERT":     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})),
		},
	}
	go h.serve()
	t.Cleanup(h.Close)
	return h
}

// Env returns the four STAGEHAND_* variables a worker needs to connect to this
// host, ready for t.Setenv.
func (h *FakeHost) Env() map[string]string {
	out := make(map[string]string, len(h.env))
	for k, v := range h.env {
		out[k] = v
	}
	return out
}

// WaitConnected blocks until a worker has been admitted (its first stream, the
// facet connection, has been accepted) or ctx ends.
func (h *FakeHost) WaitConnected(ctx context.Context) error {
	select {
	case <-h.connected:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Routes returns a client for the connected worker's Routes service.
func (h *FakeHost) Routes() hostv1.RoutesClient { return hostv1.NewRoutesClient(h.reverse()) }

// Assets returns a client for the connected worker's Assets service.
func (h *FakeHost) Assets() hostv1.AssetsClient { return hostv1.NewAssetsClient(h.reverse()) }

// Health returns a client for the connected worker's Health service.
func (h *FakeHost) Health() hostv1.HealthClient { return hostv1.NewHealthClient(h.reverse()) }

// Lifecycle returns a client for the connected worker's Lifecycle service.
func (h *FakeHost) Lifecycle() hostv1.LifecycleClient {
	return hostv1.NewLifecycleClient(h.reverse())
}

// DropWorker closes the multiplexed session, as a host that evicts a worker
// would.
func (h *FakeHost) DropWorker() {
	h.mu.Lock()
	s := h.session
	h.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// Close stops the listener and tears down the session. It is safe to call
// more than once.
func (h *FakeHost) Close() {
	if !h.closed.CompareAndSwap(false, true) {
		return
	}
	_ = h.ln.Close()
	h.mu.Lock()
	rev, s, f := h.rev, h.session, h.facetSrv
	h.mu.Unlock()
	if rev != nil {
		_ = rev.Close()
	}
	if f != nil {
		f.Stop()
	}
	if s != nil {
		_ = s.Close()
	}
}

// reverse waits for the worker and returns the cached gRPC connection to it,
// opened over one host-initiated stream (the console's ReverseChannel shape).
func (h *FakeHost) reverse() *grpc.ClientConn {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	if err := h.WaitConnected(ctx); err != nil {
		h.t.Fatalf("workertest: no worker connected: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rev != nil {
		return h.rev
	}
	session := h.session
	var once sync.Once
	dialer := func(context.Context, string) (net.Conn, error) {
		var c net.Conn
		var err error
		ran := false
		once.Do(func() { ran = true; c, err = session.Open() })
		if !ran {
			return nil, errors.New("workertest: reverse stream already consumed")
		}
		return c, err
	}
	cc, err := grpc.NewClient("passthrough:///stagehand-worker",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer))
	if err != nil {
		h.t.Fatalf("workertest: reverse client: %v", err)
	}
	h.rev = cc
	return cc
}

func (h *FakeHost) serve() {
	raw, err := h.ln.Accept()
	if err != nil {
		return
	}
	conn := tls.Server(raw, h.srvTLS)
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	session, err := yamux.Server(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	forward, err := session.AcceptStreamWithContext(ctx)
	if err != nil {
		_ = session.Close()
		return
	}
	srv := grpc.NewServer()
	for _, reg := range h.facets {
		reg(srv)
	}
	go func() { _ = srv.Serve(&oneConnListener{conn: forward, done: make(chan struct{})}) }()
	h.mu.Lock()
	h.session = session
	h.facetSrv = srv
	h.mu.Unlock()
	close(h.connected)
}

// oneConnListener hands a gRPC server the facet stream as its only connection.
type oneConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return &net.TCPAddr{} }

type leaf struct {
	tls     tls.Certificate
	certPEM string
	keyPEM  string
}

func newCA(t testing.TB) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("workertest: ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "stagehand-fake-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("workertest: ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("workertest: parse ca: %v", err)
	}
	return cert, key
}

func newLeaf(t testing.TB, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool) leaf {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("workertest: leaf key: %v", err)
	}
	usage := x509.ExtKeyUsageClientAuth
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		usage = x509.ExtKeyUsageServerAuth
		tmpl.DNSNames = []string{"localhost"}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{usage}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("workertest: leaf cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("workertest: marshal leaf key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("workertest: leaf pair: %v", err)
	}
	return leaf{tls: pair, certPEM: string(certPEM), keyPEM: string(keyPEM)}
}
