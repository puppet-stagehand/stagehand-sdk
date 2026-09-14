package host_test

import (
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// fakeDocuments satisfies hostv1.DocumentsServer with zero-value stubs,
// proving host.Host's field types are exactly the generated interfaces.
type fakeDocuments struct {
	hostv1.UnimplementedDocumentsServer
}

func TestHostIsAssignableFromGeneratedServers(t *testing.T) {
	h := &host.Host{
		Documents: &fakeDocuments{},
	}
	if h.Documents == nil {
		t.Fatal("expected Documents to be set")
	}
}
