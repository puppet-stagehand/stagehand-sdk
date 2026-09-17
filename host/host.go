// Package host defines the Go-facing shape of every facet an Expansion Pack
// may call: the v0.1.0-rc.1 tracer slice's four facets (Documents, Settings,
// Secrets, Auth) plus Inventory, added by the v0.2.0-rc.1 milestone, plus
// Code, added by the v0.3.0-rc.1 milestone. Host is exactly the generated
// gRPC server interfaces for those services — an Expansion Pack author's
// code, host/local's in-process implementation, and (later) a real
// gRPC-dialed client all satisfy this identical shape, so nothing here
// changes when real transport arrives in a later slice.
package host

import hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"

// Host is every facet an Expansion Pack may call in this SDK's scope.
type Host struct {
	Documents hostv1.DocumentsServer
	Settings  hostv1.SettingsServer
	Secrets   hostv1.SecretsServer
	Auth      hostv1.AuthServer
	Inventory hostv1.InventoryServer
	Code      hostv1.CodeServer
}
