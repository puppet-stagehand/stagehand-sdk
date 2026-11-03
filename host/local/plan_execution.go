package local

import (
	"context"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Local has no trusted live actor, explicit-instance authority, plan registry
// or executor. Even a declared grant cannot turn this stub into an executor.
// Tests needing successful responses register an explicit fake on FakeHost;
// they must not mistake a fixture for production authorization or execution.
type gatedPlanExecution struct {
	hostv1.UnimplementedPlanExecutionServer
	perms map[string]bool
}

func (g *gatedPlanExecution) unavailable() error {
	if !g.perms["bolt:run"] {
		return ErrPermissionDenied("bolt:run")
	}
	return status.Error(codes.Unimplemented, "PlanExecution requires a host implementation with live actor and scoped execution authorization")
}
func (g *gatedPlanExecution) ListCandidates(context.Context, *hostv1.ListPlanCandidatesRequest) (*hostv1.ListPlanCandidatesResponse, error) {
	return nil, g.unavailable()
}
func (g *gatedPlanExecution) Preview(context.Context, *hostv1.PreviewPlanRequest) (*hostv1.PlanPreview, error) {
	return nil, g.unavailable()
}
func (g *gatedPlanExecution) Start(context.Context, *hostv1.StartPlanRequest) (*hostv1.PlanRun, error) {
	return nil, g.unavailable()
}
func (g *gatedPlanExecution) Get(context.Context, *hostv1.GetPlanRunRequest) (*hostv1.PlanRun, error) {
	return nil, g.unavailable()
}
