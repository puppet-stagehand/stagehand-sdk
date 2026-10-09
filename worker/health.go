package worker

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

type healthServer struct {
	hostv1.UnimplementedHealthServer
	ready func(context.Context) (bool, string)
}

func (h *healthServer) Check(ctx context.Context, _ *emptypb.Empty) (*hostv1.HealthStatus, error) {
	return &hostv1.HealthStatus{Live: true, Ready: true}, nil
}
