package worker

import (
	"bytes"
	"context"
	"net/http"
	"net/url"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

type routesServer struct {
	hostv1.UnimplementedRoutesServer
	handler http.Handler
	maxBody int64
}

func (r *routesServer) Dispatch(ctx context.Context, req *hostv1.HttpRequest) (*hostv1.HttpResponse, error) {
	if r.handler == nil {
		return &hostv1.HttpResponse{Status: http.StatusNotFound, Body: []byte("not found")}, nil
	}
	u := &url.URL{Path: "/" + req.GetPath()}
	q := url.Values{}
	for k, v := range req.GetQuery() {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	hr, err := http.NewRequestWithContext(ctx, req.GetMethod(), u.String(), bytes.NewReader(req.GetBody()))
	if err != nil {
		return &hostv1.HttpResponse{Status: http.StatusBadRequest, Body: []byte("bad request")}, nil
	}
	for k, v := range req.GetHeaders() {
		hr.Header.Set(k, v)
	}
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	r.handler.ServeHTTP(rec, hr)
	headers := map[string]string{}
	for k := range rec.header {
		headers[k] = rec.header.Get(k)
	}
	return &hostv1.HttpResponse{Status: int32(rec.status), Headers: headers, Body: rec.body.Bytes()}, nil
}

// recorder is the in-package http.ResponseWriter the handler writes to.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.status = code }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

// PrincipalFrom returns the principal the host resolved for the request being
// handled. It is only ever set from the host's HttpRequest.principal field.
func PrincipalFrom(ctx context.Context) (*hostv1.Principal, bool) {
	return nil, false
}
