package worker

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// principalKey is the unexported context key under which the host-resolved
// principal travels. Nothing outside this package can set it, and nothing in
// this package sets it from anything but HttpRequest.principal.
type principalKey struct{}

// PrincipalFrom returns the principal the host resolved for the request being
// handled (a session user or a verified token). It reads only the principal
// field of the host's HttpRequest; request headers are never consulted, so a
// browser-supplied header cannot impersonate a principal. ok is false when the
// host sent no principal.
func PrincipalFrom(ctx context.Context) (*hostv1.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*hostv1.Principal)
	return p, ok && p != nil
}

// hostName is the Host the handler sees. The console is the only caller,
// so there is no real host name to present.
const hostName = "stagehand-worker"

type routesServer struct {
	hostv1.UnimplementedRoutesServer
	handler http.Handler
	maxBody int64
}

func textResponse(status int, msg string) *hostv1.HttpResponse {
	return &hostv1.HttpResponse{
		Status:  int32(status),
		Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:    []byte(msg),
	}
}

// Dispatch adapts one HttpRequest to the pack's http.Handler.
func (r *routesServer) Dispatch(ctx context.Context, req *hostv1.HttpRequest) (*hostv1.HttpResponse, error) {
	if r.handler == nil {
		return textResponse(http.StatusNotFound, "not found"), nil
	}
	if int64(len(req.GetBody())) > r.maxBody {
		return textResponse(http.StatusRequestEntityTooLarge, "request body too large"), nil
	}
	hr, ok := buildRequest(ctx, req)
	if !ok {
		return textResponse(http.StatusBadRequest, "bad request"), nil
	}

	rec := &recorder{header: http.Header{}, limit: r.maxBody}
	if panicked := serve(r.handler, rec, hr); panicked {
		return textResponse(http.StatusInternalServerError, "internal error"), nil
	}
	switch {
	case rec.overflow:
		return textResponse(http.StatusBadGateway, "response body too large"), nil
	case rec.badStatus:
		return textResponse(http.StatusBadGateway, "invalid response status"), nil
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	headers := make(map[string]string, len(rec.header))
	for k, v := range rec.header {
		headers[k] = strings.Join(v, ", ")
	}
	return &hostv1.HttpResponse{Status: int32(status), Headers: headers, Body: rec.body.Bytes()}, nil
}

// buildRequest turns the proto request into a server-side *http.Request. It
// refuses (ok=false) a method that is not an HTTP token, and a path with a
// control character or a ".." segment, before the handler sees anything.
func buildRequest(ctx context.Context, req *hostv1.HttpRequest) (*http.Request, bool) {
	p := req.GetPath()
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return nil, false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return nil, false
		}
	}
	if req.GetMethod() == "" {
		return nil, false
	}
	u := &url.URL{Path: "/" + p}
	q := url.Values{}
	for k, v := range req.GetQuery() {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	if pr := req.GetPrincipal(); pr != nil {
		ctx = context.WithValue(ctx, principalKey{}, pr)
	}
	var body *bytes.Reader
	if len(req.GetBody()) > 0 {
		body = bytes.NewReader(req.GetBody())
	}
	var hr *http.Request
	var err error
	if body == nil {
		hr, err = http.NewRequestWithContext(ctx, req.GetMethod(), u.String(), http.NoBody)
	} else {
		hr, err = http.NewRequestWithContext(ctx, req.GetMethod(), u.String(), body)
	}
	if err != nil {
		return nil, false
	}
	for k, v := range req.GetHeaders() {
		hr.Header.Set(k, v)
	}
	hr.Host = hostName
	hr.RequestURI = u.RequestURI()
	hr.ContentLength = int64(len(req.GetBody()))
	return hr, true
}

// serve runs the handler and contains a panic: the worker must survive a bug
// in one route. The panic value is logged to the worker's own log (never sent
// to the caller) together with the stack.
func serve(h http.Handler, w http.ResponseWriter, r *http.Request) (panicked bool) {
	defer func() {
		if v := recover(); v != nil {
			if !errors.Is(asError(v), http.ErrAbortHandler) {
				log.Printf("worker: route handler panic: %v\n%s", v, debug.Stack())
			}
			panicked = true
		}
	}()
	h.ServeHTTP(w, r)
	return false
}

func asError(v any) error {
	err, _ := v.(error)
	return err
}

// errResponseTooLarge is what a handler's Write returns once the response
// would exceed the cap, so a well-behaved handler stops.
var errResponseTooLarge = errors.New("worker: response body exceeds MaxBodyBytes")

// recorder is the in-package http.ResponseWriter a route handler writes to. It
// stops buffering at the cap instead of measuring afterwards.
type recorder struct {
	header    http.Header
	status    int
	body      bytes.Buffer
	limit     int64
	overflow  bool
	badStatus bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.status != 0 {
		return
	}
	switch {
	case code >= 100 && code < 200:
		return // informational: not the final status
	case code < 200 || code > 599:
		r.badStatus = true
		r.status = http.StatusBadGateway
	default:
		r.status = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if r.overflow || int64(r.body.Len())+int64(len(b)) > r.limit {
		r.overflow = true
		return 0, errResponseTooLarge
	}
	return r.body.Write(b)
}

// Flush satisfies http.Flusher; the response is returned whole, so there is
// nothing to flush.
func (r *recorder) Flush() {}
