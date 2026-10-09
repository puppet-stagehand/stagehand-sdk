package worker

import (
	"context"
	"errors"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

type lifecycleServer struct {
	hostv1.UnimplementedLifecycleServer
	onShutdown func(context.Context) error
	onPurge    func(context.Context) error
	stop       func()
}

// Shutdown runs OnShutdown, then stops Run. The worker stops even when the
// hook fails or panics; the host is told about the failure.
func (l *lifecycleServer) Shutdown(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	err := callHook(ctx, "OnShutdown", l.onShutdown)
	l.stop()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}

// Purge runs OnPurge. Without a hook it succeeds. A failing hook is Internal,
// so the host does not believe the pack cleaned up when it did not.
func (l *lifecycleServer) Purge(ctx context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	if err := callHook(ctx, "OnPurge", l.onPurge); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}

// callHook runs an optional hook and turns a panic into an error that does not
// carry the panic value (which is logged locally instead).
func callHook(ctx context.Context, name string, f func(context.Context) error) (err error) {
	if f == nil {
		return nil
	}
	defer func() {
		if v := recover(); v != nil {
			log.Printf("worker: %s hook panic: %v", name, v)
			err = errors.New(name + " failed")
		}
	}()
	return f(ctx)
}
