// Package host defines the Go-facing shape of the v0.1.0-rc.1 tracer
// slice's four in-scope facets (Documents, Settings, Secrets, Auth). Host
// is exactly the generated gRPC server interfaces for those services — an
// Expansion Pack author's code, host/local's in-process implementation,
// and (later) a real gRPC-dialed client all satisfy this identical shape,
// so nothing here changes when real transport arrives in a later slice.
package host

import hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"

// Host is every facet an Expansion Pack may call in this slice's scope.
type Host struct {
	Documents hostv1.DocumentsServer
	Settings  hostv1.SettingsServer
	Secrets   hostv1.SecretsServer
	Auth      hostv1.AuthServer
}
