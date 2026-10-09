package worker

import (
	"context"
	"log"

	"google.golang.org/protobuf/types/known/emptypb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

type healthServer struct {
	hostv1.UnimplementedHealthServer
	ready func(context.Context) (bool, string)
}

// Check reports live (the process answers) and ready (Options.Ready, or true
// without one). A panicking Ready hook reads as not ready; the panic value is
// logged locally and never sent to the host.
func (h *healthServer) Check(ctx context.Context, _ *emptypb.Empty) (*hostv1.HealthStatus, error) {
	st := &hostv1.HealthStatus{Live: true, Ready: true}
	if h.ready == nil {
		return st, nil
	}
	st.Ready, st.Detail = callReady(ctx, h.ready)
	return st, nil
}

func callReady(ctx context.Context, f func(context.Context) (bool, string)) (ready bool, detail string) {
	defer func() {
		if v := recover(); v != nil {
			log.Printf("worker: Ready hook panic: %v", v)
			ready, detail = false, "readiness check failed"
		}
	}()
	return f(ctx)
}
