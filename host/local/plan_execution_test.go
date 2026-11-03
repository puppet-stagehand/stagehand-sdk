package local_test

import (
	"context"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestPlanExecutionFailClosedMethodCoverage(t *testing.T) {
	calls := map[string]func(hostv1.PlanExecutionServer) error{
		"ListCandidates": func(s hostv1.PlanExecutionServer) error {
			_, e := s.ListCandidates(context.Background(), &hostv1.ListPlanCandidatesRequest{InstanceId: "inst-a"})
			return e
		},
		"Preview": func(s hostv1.PlanExecutionServer) error {
			_, e := s.Preview(context.Background(), &hostv1.PreviewPlanRequest{})
			return e
		},
		"Start": func(s hostv1.PlanExecutionServer) error {
			_, e := s.Start(context.Background(), &hostv1.StartPlanRequest{})
			return e
		},
		"Get": func(s hostv1.PlanExecutionServer) error {
			_, e := s.Get(context.Background(), &hostv1.GetPlanRunRequest{})
			return e
		},
	}
	svc := hostv1.File_stagehand_host_v1_host_proto.Services().ByName("PlanExecution")
	if svc == nil || svc.Methods().Len() != len(calls) {
		t.Fatal("every PlanExecution RPC must have a fail-closed case")
	}
	for i := 0; i < svc.Methods().Len(); i++ {
		if calls[string(svc.Methods().Get(i).Name())] == nil {
			t.Fatal("uncovered RPC")
		}
	}
	for _, granted := range []bool{false, true} {
		var permissions []string
		want := codes.PermissionDenied
		if granted {
			permissions = []string{"bolt:run"}
			want = codes.Unimplemented
		}
		h := local.New(permissions, "pack-a")
		if h.PlanExecution == nil {
			t.Fatal("missing PlanExecution facet")
		}
		for name, call := range calls {
			if got := status.Code(call(h.PlanExecution)); got != want {
				t.Errorf("%s grant=%v got=%v want=%v", name, granted, got, want)
			}
		}
	}
}

func TestPlanExecutionStartCannotReplacePreviewAuthority(t *testing.T) {
	fields := (&hostv1.StartPlanRequest{}).ProtoReflect().Descriptor().Fields()
	if fields.Len() != 2 || string(fields.Get(0).Name()) != "preview_id" || string(fields.Get(1).Name()) != "idempotency_key" {
		t.Fatal("Start must accept only preview ID and idempotency key")
	}
}
