package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// dialTimeout bounds connecting to the host and being admitted by it.
const dialTimeout = 20 * time.Second

// hostEnv is the worker's environment contract, read once.
type hostEnv struct {
	addr string
	cert string
	key  string
	ca   string
}

// envNames lists the contract variables in the order they are checked.
var envNames = []string{EnvHostAddr, EnvClientCert, EnvClientKey, EnvCACert}

// readEnv reads the four variables. A missing one is an error that names the
// variable and never any variable's value.
func readEnv() (hostEnv, error) {
	e := hostEnv{
		addr: os.Getenv(EnvHostAddr),
		cert: os.Getenv(EnvClientCert),
		key:  os.Getenv(EnvClientKey),
		ca:   os.Getenv(EnvCACert),
	}
	var missing []string
	for _, p := range []struct{ name, val string }{
		{EnvHostAddr, e.addr}, {EnvClientCert, e.cert}, {EnvClientKey, e.key}, {EnvCACert, e.ca},
	} {
		if p.val == "" {
			missing = append(missing, p.name)
		}
	}
	if len(missing) > 0 {
		return hostEnv{}, fmt.Errorf("worker: required environment variable not set: %s", strings.Join(missing, ", "))
	}
	return e, nil
}

// redact removes every contract value from a message so no error the runtime
// returns can echo key material, certificates or the host address.
func (e hostEnv) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, p := range []struct{ name, val string }{
		{EnvClientKey, e.key}, {EnvClientCert, e.cert}, {EnvCACert, e.ca}, {EnvHostAddr, e.addr},
	} {
		if p.val != "" {
			msg = strings.ReplaceAll(msg, p.val, "<"+p.name+">")
		}
	}
	return &redactedError{msg: msg, err: err}
}

type redactedError struct {
	msg string
	err error
}

func (r *redactedError) Error() string { return r.msg }
func (r *redactedError) Unwrap() error { return r.err }

// tlsConfig builds the client TLS configuration from the PEM content.
func (e hostEnv) tlsConfig() (*tls.Config, error) {
	pair, err := tls.X509KeyPair([]byte(e.cert), []byte(e.key))
	if err != nil {
		return nil, e.redact(fmt.Errorf("worker: %s and %s are not a valid PEM certificate and key pair: %w", EnvClientCert, EnvClientKey, err))
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(e.ca)) {
		return nil, fmt.Errorf("worker: %s holds no PEM certificate", EnvCACert)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,

		// ALPN h2 is required: the console's gRPC server credentials reject a
		// handshake without it, and the worker speaks the multiplexed session
		// over the raw TLS connection instead of letting grpc-go dial.
		NextProtos: []string{"h2"},

		MinVersion: tls.VersionTLS12,
	}, nil
}

// dial opens the mTLS connection and starts the client side of the
// multiplexed session on it.
func (e hostEnv) dial(ctx context.Context, cfg *tls.Config) (*yamux.Session, error) {
	raw, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", e.addr)
	if err != nil {
		return nil, e.redact(fmt.Errorf("worker: connect to %s: %w", EnvHostAddr, err))
	}
	ycfg := yamux.DefaultConfig()
	ycfg.LogOutput = io.Discard
	session, err := yamux.Client(raw, ycfg)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("worker: start multiplexed session: %w", err)
	}
	return session, nil
}

// sessionListener adapts the session's inbound streams (the host's reverse
// connection) to a net.Listener a gRPC server can serve.
type sessionListener struct{ session *yamux.Session }

func (l *sessionListener) Accept() (net.Conn, error) { return l.session.Accept() }
func (l *sessionListener) Close() error              { return l.session.Close() }
func (l *sessionListener) Addr() net.Addr            { return l.session.Addr() }

// openForward opens the first stream of the session as the facet connection.
// The host's registrar waits for exactly this stream to finish admission.
// The stream already runs inside mTLS, so the gRPC client on top of it uses
// insecure transport credentials.
func openForward(session *yamux.Session) (*grpc.ClientConn, error) {
	forward, err := session.Open()
	if err != nil {
		return nil, fmt.Errorf("worker: open facet stream: %w", err)
	}
	var once sync.Once
	dialer := func(context.Context, string) (net.Conn, error) {
		var c net.Conn
		once.Do(func() { c = forward })
		if c == nil {
			return nil, errors.New("worker: facet stream already consumed")
		}
		return c, nil
	}
	cc, err := grpc.NewClient("passthrough:///stagehand-host",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer))
	if err != nil {
		_ = forward.Close()
		return nil, fmt.Errorf("worker: facet client: %w", err)
	}
	return cc, nil
}

// waitReady connects cc and waits until the host's server answers on it, which
// is also how a refused admission shows up (the host closes the session).
func waitReady(ctx context.Context, cc *grpc.ClientConn) error {
	cc.Connect()
	for {
		switch s := cc.GetState(); s {
		case connectivity.Ready:
			return nil
		case connectivity.TransientFailure, connectivity.Shutdown:
			return errors.New("worker: the host did not accept the facet connection")
		default:
			if !cc.WaitForStateChange(ctx, s) {
				return fmt.Errorf("worker: waiting for the host to accept the facet connection: %w", ctx.Err())
			}
		}
	}
}
