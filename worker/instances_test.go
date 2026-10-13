package worker_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/worker"
	"github.com/puppet-stagehand/stagehand-sdk/worker/workertest"
)

// fakeInstances answers the worker's Instances calls with one fixed estate.
type fakeInstances struct {
	hostv1.UnimplementedInstancesServer
}

var fakeEstate = &hostv1.Instance{
	Id:        "inst-00000000000000aa",
	Name:      "Fake estate",
	Edition:   "core",
	Endpoints: []*hostv1.InstanceEndpoint{{Kind: "puppetserver", Host: "primary.example.test", Port: 8140}},
	CaCertPem: "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n",
}

func (fakeInstances) ListInstances(context.Context, *hostv1.ListInstancesRequest) (*hostv1.ListInstancesResponse, error) {
	return &hostv1.ListInstancesResponse{Instances: []*hostv1.Instance{fakeEstate}, Page: &hostv1.PageInfo{}}, nil
}

func (fakeInstances) GetInstance(_ context.Context, r *hostv1.GetInstanceRequest) (*hostv1.Instance, error) {
	if r.GetId() != fakeEstate.Id {
		return nil, status.Errorf(codes.NotFound, "no instance %q", r.GetId())
	}
	return fakeEstate, nil
}

// TestWorkerClientsIncludeInstances: the Clients a worker receives carry an
// Instances client on the forward connection, and it reaches the host's
// Instances server.
func TestWorkerClientsIncludeInstances(t *testing.T) {
	host := workertest.NewFakeHost(t, func(s grpc.ServiceRegistrar) { hostv1.RegisterInstancesServer(s, fakeInstances{}) })

	type result struct {
		list    *hostv1.ListInstancesResponse
		missing error
		err     error
	}
	got := make(chan result, 1)
	start(t, host, worker.Options{
		OnConnected: func(ctx context.Context, c *worker.Clients) error {
			if c.Instances == nil {
				got <- result{err: errNilClient}
				return nil
			}
			list, err := c.Instances.ListInstances(ctx, &hostv1.ListInstancesRequest{})
			_, missing := c.Instances.GetInstance(ctx, &hostv1.GetInstanceRequest{Id: "inst-gone"})
			got <- result{list: list, missing: missing, err: err}
			return nil
		},
	})

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("Instances.ListInstances: %v", r.err)
		}
		if len(r.list.GetInstances()) != 1 || r.list.GetInstances()[0].GetId() != fakeEstate.Id {
			t.Fatalf("ListInstances = %v, want the fake estate", r.list)
		}
		if status.Code(r.missing) != codes.NotFound {
			t.Fatalf("GetInstance(unknown) = %v, want NotFound", r.missing)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnConnected was not called")
	}
}

type nilClientError struct{}

func (nilClientError) Error() string { return "worker.Clients.Instances is nil" }

var errNilClient error = nilClientError{}
