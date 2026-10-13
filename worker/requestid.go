package worker

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// RequestIDMetadataKey is the gRPC metadata key under which the worker sends
// the host's request id on facet calls made while handling a proxied request.
const RequestIDMetadataKey = "stagehand-request-id"

// maxRequestIDLen bounds the request id the worker will carry. A longer value
// from the host is dropped, not truncated.
const maxRequestIDLen = 128

// requestIDKey is the unexported context key under which the request id
// travels. Nothing outside this package can set it, and nothing in this
// package sets it from anything but HttpRequest.request_id.
type requestIDKey struct{}

// RequestIDFrom returns the id the host gave the request being handled, or ""
// when ctx carries none. Every ctx derived from the route handler's request
// ctx (context.WithTimeout, context.WithValue, and so on) carries it, and the
// worker sends it with every facet call made with such a ctx, so the console
// can tell which person stands behind the call.
//
// The worker never invents an id. A facet call from a ctx that did not come
// from a request (OnConnected's ctx, context.Background(), a goroutine
// started on its own) carries none, and the console attributes it to no
// person: an approval proposal filed that way records started_by = none.
// File proposals inside the route handler, with its ctx. A pack that hands
// work to another goroutine and wants the console to attribute it to the
// person must pass this ctx (or one derived from it) along; the console may
// stop honouring an id once the request has returned.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// withRequestID returns ctx carrying id, when id is a well-formed request id.
// A value that is empty, longer than maxRequestIDLen bytes or has a byte
// outside [A-Za-z0-9._-] is dropped (ctx is returned unchanged), so a
// malformed host value never reaches the wire or a log line.
func withRequestID(ctx context.Context, id string) context.Context {
	if !validRequestID(id) {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// outgoingWithRequestID adds the ctx's request id to its outgoing metadata,
// replacing any value already set under RequestIDMetadataKey so exactly one
// is sent. Without an id it returns ctx untouched.
func outgoingWithRequestID(ctx context.Context) context.Context {
	id := RequestIDFrom(ctx)
	if id == "" {
		return ctx
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(RequestIDMetadataKey, id)
	return metadata.NewOutgoingContext(ctx, md)
}

// requestIDUnaryInterceptor sends the request id on unary facet calls.
func requestIDUnaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	return invoker(outgoingWithRequestID(ctx), method, req, reply, cc, opts...)
}

// requestIDStreamInterceptor sends the request id when a streaming facet call
// opens its stream.
func requestIDStreamInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return streamer(outgoingWithRequestID(ctx), desc, cc, method, opts...)
}
