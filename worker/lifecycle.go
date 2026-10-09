package worker

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

type lifecycleServer struct {
	hostv1.UnimplementedLifecycleServer
	onShutdown func(context.Context) error
	onPurge    func(context.Context) error
	stop       func()
}

func (l *lifecycleServer) Shutdown(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	l.stop()
	return &emptypb.Empty{}, nil
}

func (l *lifecycleServer) Purge(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
