package worker

import (
	"google.golang.org/grpc"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// newClients builds every generated facet client on the one forward
// connection. Permissions are declared in the pack's manifest and enforced by
// the host on each call: a client here exists for every facet, whether or not
// the pack may use it, and the SDK does not pretend otherwise.
func newClients(cc grpc.ClientConnInterface) *Clients {
	return &Clients{
		Documents: hostv1.NewDocumentsClient(cc),
		Settings:  hostv1.NewSettingsClient(cc),
		Secrets:   hostv1.NewSecretsClient(cc),
		Auth:      hostv1.NewAuthClient(cc),
		Inventory: hostv1.NewInventoryClient(cc),
		Code:      hostv1.NewCodeClient(cc),
		Forge:     hostv1.NewForgeClient(cc),
	}
}
