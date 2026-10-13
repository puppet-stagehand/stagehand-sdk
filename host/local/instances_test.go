package local_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

// The two estates these tests seed. Ids follow the console's "inst-" + hex
// shape; the endpoints are the three kinds the host exposes (SDK-01).
func instanceA() *hostv1.Instance {
	return &hostv1.Instance{
		Id:      "inst-0a0a0a0a0a0a0a0a",
		Name:    "PE production",
		Edition: "pe",
		Endpoints: []*hostv1.InstanceEndpoint{
			{Kind: "puppetserver", Host: "pe-primary.example.test", Port: 8140},
			{Kind: "puppetdb", Host: "pe-primary.example.test", Port: 8081},
			{Kind: "classifier", Host: "pe-primary.example.test", Port: 4433},
		},
		CaCertPem: "-----BEGIN CERTIFICATE-----\nMIIBfixture\n-----END CERTIFICATE-----\n",
		Active:    true,
	}
}

func instanceB() *hostv1.Instance {
	return &hostv1.Instance{
		Id:      "inst-0b0b0b0b0b0b0b0b",
		Name:    "Core lab",
		Edition: "core",
		Endpoints: []*hostv1.InstanceEndpoint{
			{Kind: "puppetserver", Host: "core-primary.example.test", Port: 8140},
		},
		CaCertPem: "-----BEGIN CERTIFICATE-----\nMIIBcore\n-----END CERTIFICATE-----\n",
	}
}

func wantInstancesCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %v (%v), want %v", got, err, want)
	}
}

// TestInstancesFacet_ListAndGet: seeded estates list in a stable order (by
// id) with paging, GetInstance returns a copy of the seeded estate, and an
// unknown id is NOT_FOUND.
func TestInstancesFacet_ListAndGet(t *testing.T) {
	ctx := context.Background()
	// Seeded out of order on purpose: listing order is by id, not seed order.
	h := local.New([]string{"instances:read"}, "pk", local.WithInstances(instanceB(), instanceA()))

	all, err := h.Instances.ListInstances(ctx, &hostv1.ListInstancesRequest{})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if got := len(all.GetInstances()); got != 2 {
		t.Fatalf("ListInstances returned %d instances, want 2", got)
	}
	if all.GetInstances()[0].GetId() != instanceA().Id || all.GetInstances()[1].GetId() != instanceB().Id {
		t.Fatalf("order = [%s %s], want [%s %s]", all.GetInstances()[0].GetId(), all.GetInstances()[1].GetId(), instanceA().Id, instanceB().Id)
	}
	if all.GetPage().GetNextCursor() != "" {
		t.Fatalf("one page holds both, next_cursor = %q, want empty", all.GetPage().GetNextCursor())
	}

	// Paging: one per page, the cursor walks to the second, then stops.
	p1, err := h.Instances.ListInstances(ctx, &hostv1.ListInstancesRequest{Page: &hostv1.Page{Limit: 1}})
	if err != nil {
		t.Fatalf("ListInstances page 1: %v", err)
	}
	if len(p1.GetInstances()) != 1 || p1.GetInstances()[0].GetId() != instanceA().Id || p1.GetPage().GetNextCursor() == "" {
		t.Fatalf("page 1 = %v, want [%s] with a next cursor", p1, instanceA().Id)
	}
	p2, err := h.Instances.ListInstances(ctx, &hostv1.ListInstancesRequest{Page: &hostv1.Page{Limit: 1, Cursor: p1.GetPage().GetNextCursor()}})
	if err != nil {
		t.Fatalf("ListInstances page 2: %v", err)
	}
	if len(p2.GetInstances()) != 1 || p2.GetInstances()[0].GetId() != instanceB().Id || p2.GetPage().GetNextCursor() != "" {
		t.Fatalf("page 2 = %v, want [%s] and no next cursor", p2, instanceB().Id)
	}
	_, err = h.Instances.ListInstances(ctx, &hostv1.ListInstancesRequest{Page: &hostv1.Page{Cursor: "not-a-cursor"}})
	wantInstancesCode(t, err, codes.InvalidArgument)

	got, err := h.Instances.GetInstance(ctx, &hostv1.GetInstanceRequest{Id: instanceA().Id})
	if err != nil {
		t.Fatalf("GetInstance(a): %v", err)
	}
	if !proto.Equal(got, instanceA()) {
		t.Fatalf("GetInstance(a) = %v, want %v", got, instanceA())
	}
	// The host hands out copies: a caller mutating its answer never changes
	// what the next caller reads.
	got.Name = "mutated"
	got.Endpoints[0].Host = "evil.example.test"
	again, err := h.Instances.GetInstance(ctx, &hostv1.GetInstanceRequest{Id: instanceA().Id})
	if err != nil {
		t.Fatalf("GetInstance(a) again: %v", err)
	}
	if !proto.Equal(again, instanceA()) {
		t.Fatalf("a caller's mutation leaked into the store: %v", again)
	}

	_, err = h.Instances.GetInstance(ctx, &hostv1.GetInstanceRequest{Id: "inst-missing"})
	wantInstancesCode(t, err, codes.NotFound)
	_, err = h.Instances.GetInstance(ctx, &hostv1.GetInstanceRequest{})
	wantInstancesCode(t, err, codes.InvalidArgument)

	// No WithInstances at all: an empty estate list, not an error.
	empty := local.New([]string{"instances:read"}, "pk")
	none, err := empty.Instances.ListInstances(ctx, &hostv1.ListInstancesRequest{})
	if err != nil {
		t.Fatalf("ListInstances with no seed: %v", err)
	}
	if len(none.GetInstances()) != 0 {
		t.Fatalf("no seed listed %d instances, want 0", len(none.GetInstances()))
	}
}

// TestInstancesFacet_PermissionRequired: without instances:read both RPCs are
// refused with the same PERMISSION_DENIED + facet_not_declared shape every
// other gated facet uses.
func TestInstancesFacet_PermissionRequired(t *testing.T) {
	ctx := context.Background()
	for label, perms := range map[string][]string{
		"no-perms":       nil,
		"inventory-only": {"inventory:rw", "inventory:read"},
	} {
		h := local.New(perms, "pk", local.WithInstances(instanceA()))
		calls := map[string]func() error{
			"ListInstances": func() error {
				_, err := h.Instances.ListInstances(ctx, &hostv1.ListInstancesRequest{})
				return err
			},
			"GetInstance": func() error {
				_, err := h.Instances.GetInstance(ctx, &hostv1.GetInstanceRequest{Id: instanceA().Id})
				return err
			},
		}
		for name, call := range calls {
			err := call()
			st, _ := status.FromError(err)
			if st.Code() != codes.PermissionDenied {
				t.Fatalf("%s/%s: code = %v (%v), want PermissionDenied", label, name, st.Code(), err)
			}
			found := false
			for _, d := range st.Details() {
				ed, ok := d.(*hostv1.ErrorDetail)
				if !ok {
					continue
				}
				found = true
				if ed.Code != "facet_not_declared" || !strings.Contains(ed.Message, "instances:read") || !strings.Contains(ed.Fix, "instances:read") {
					t.Fatalf("%s/%s: ErrorDetail = %+v, want facet_not_declared naming instances:read", label, name, ed)
				}
			}
			if !found {
				t.Fatalf("%s/%s: no *hostv1.ErrorDetail in status details", label, name)
			}
		}
	}
}

// TestInstancesFacet_RemovedInstanceIsNotFound proves the D-19 behaviour packs
// build on: once the console removes an estate, GetInstance answers NOT_FOUND
// and ListInstances omits it.
func TestInstancesFacet_RemovedInstanceIsNotFound(t *testing.T) {
	ctx := context.Background()
	h := local.New([]string{"instances:read"}, "pk", local.WithInstances(instanceA(), instanceB()))

	if !local.RemoveInstance(h, instanceB().Id) {
		t.Fatal("RemoveInstance(b) = false, want true")
	}
	if local.RemoveInstance(h, instanceB().Id) {
		t.Fatal("RemoveInstance(b) twice = true, want false the second time")
	}
	_, err := h.Instances.GetInstance(ctx, &hostv1.GetInstanceRequest{Id: instanceB().Id})
	wantInstancesCode(t, err, codes.NotFound)

	list, err := h.Instances.ListInstances(ctx, &hostv1.ListInstancesRequest{})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(list.GetInstances()) != 1 || list.GetInstances()[0].GetId() != instanceA().Id {
		t.Fatalf("after removal ListInstances = %v, want only %s", list.GetInstances(), instanceA().Id)
	}

	// A host built by something other than local.New is left alone.
	if local.RemoveInstance(nil, instanceA().Id) {
		t.Fatal("RemoveInstance(nil host) = true, want false")
	}
}

// instancesAllowedFields is every field name the Instances contract may carry
// (D-14). A new field is a contract change that must be reviewed here first.
var instancesAllowedFields = map[string]bool{
	"id": true, "name": true, "edition": true, "endpoints": true, "ca_cert_pem": true, "active": true,
	"kind": true, "host": true, "port": true, "page": true, "instances": true,
}

// credentialLike matches a field name that could carry a credential or a path
// on the console host. ca_cert_pem (the PUBLIC CA bundle) is the one allowed
// name that mentions a certificate.
var credentialLike = regexp.MustCompile(`(?i)key|cert_path|token|secret|password|credential`)

// TestInstancesMessagesCarryNoCredential walks the descriptors of every
// message the Instances service sends or receives (T-57.6-01). It fails if a
// field name falls outside the allow-list or looks like a credential, so a
// later edit cannot quietly widen the facet into one that hands a pack the
// console's key, client certificate, token, password or a host file path.
func TestInstancesMessagesCarryNoCredential(t *testing.T) {
	svc := hostv1.File_stagehand_host_v1_host_proto.Services().ByName("Instances")
	if svc == nil {
		t.Fatal("service Instances is missing from host.proto")
	}
	// Page and PageInfo are the shared paging messages from common.proto
	// (cursor, limit, next_cursor); they are checked by name, not walked.
	shared := map[protoreflect.FullName]bool{
		(&hostv1.Page{}).ProtoReflect().Descriptor().FullName():     true,
		(&hostv1.PageInfo{}).ProtoReflect().Descriptor().FullName(): true,
	}

	seen := map[protoreflect.FullName]bool{}
	var walk func(md protoreflect.MessageDescriptor)
	walk = func(md protoreflect.MessageDescriptor) {
		if seen[md.FullName()] || shared[md.FullName()] {
			return
		}
		seen[md.FullName()] = true
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			name := string(f.Name())
			if !instancesAllowedFields[name] {
				t.Errorf("%s.%s is not in the Instances allow-list (D-14: no console credential may cross this facet)", md.FullName(), name)
			}
			if name != "ca_cert_pem" && credentialLike.MatchString(name) {
				t.Errorf("%s.%s looks like a credential field (D-14)", md.FullName(), name)
			}
			if f.Kind() == protoreflect.MessageKind || f.Kind() == protoreflect.GroupKind {
				walk(f.Message())
			}
		}
	}
	methods := svc.Methods()
	for i := 0; i < methods.Len(); i++ {
		walk(methods.Get(i).Input())
		walk(methods.Get(i).Output())
	}

	for _, want := range []protoreflect.FullName{
		"stagehand.host.v1.Instance",
		"stagehand.host.v1.InstanceEndpoint",
		"stagehand.host.v1.ListInstancesRequest",
		"stagehand.host.v1.ListInstancesResponse",
		"stagehand.host.v1.GetInstanceRequest",
	} {
		if !seen[want] {
			t.Errorf("%s was not reached from service Instances", want)
		}
	}
	if methods.Len() != 2 {
		t.Errorf("service Instances has %d RPCs, want exactly ListInstances and GetInstance (read-only, D-14)", methods.Len())
	}
}
