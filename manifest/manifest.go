// Package manifest models and validates an Expansion Pack manifest
// (manifest/schema.json, contract_version 1).
//
// The JSON Schema is the source of truth for tooling; this package encodes
// the same rules in Go so `pack-check` runs with no external dependency and
// emits findings with stable codes and a one-line fix.
package manifest

// ContractVersion is the contract this SDK build targets.
const ContractVersion = 1

// NavGroups are the seven shipped console nav groups. Packs add items only.
var NavGroups = []string{"Overview", "Inventory", "Reporting", "Automation", "Configuration", "Patching & Compliance", "Administration"}

// Slots the console can mount a pack component into.
var Slots = []string{"page", "settingsPanel", "nodeDetailTab", "deviceDetailTab", "inventoryKindView", "complianceSourceCard", "dashboardCard"}

// ExtensionPoints a pack may contribute to.
var ExtensionPoints = []string{"patching.provider", "compliance.source"}

// Permissions are host facets/scopes. Two are parameterised:
// inventory:kind:<name> and legacy:read:<table>.
var Permissions = []string{
	"documents:rw", "secrets:rw", "bolt:run", "inventory:read", "inventory:rw", "compliance:ingest",
	"tokens:issue", "forge:read", "forge:rw", "forge:recommend", "puppetdb:read", "classification:read", "code:read", "code:rw", "code:import", "activity:read",
}

type Nav struct {
	Group string `json:"group"`
	Label string `json:"label"`
	Route string `json:"route"`
}

type Access struct {
	Role   string `json:"role,omitempty"`
	Scope  string `json:"scope,omitempty"`
	Public bool   `json:"public,omitempty"`
}

type Route struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Access      Access `json:"access"`
	OperationID string `json:"operation_id"`
}

type Job struct {
	Name      string `json:"name"`
	Every     string `json:"every"`
	Singleton bool   `json:"singleton,omitempty"`
}

type ExtensionPoint struct {
	Point string `json:"point"`
	ID    string `json:"id"`
}

type Content struct {
	ForgeSlug   string `json:"forge_slug"`
	Version     string `json:"version"`
	RunnerClass string `json:"runner_class,omitempty"`
}

type Volume struct {
	Name string `json:"name"`
	Size string `json:"size"`
}

type Resources struct {
	CPU     string   `json:"cpu,omitempty"`
	Memory  string   `json:"memory,omitempty"`
	Volumes []Volume `json:"volumes,omitempty"`
}

type Network struct {
	Egress []string `json:"egress,omitempty"`
}

type Manifest struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Version         string           `json:"version"`
	ContractVersion int              `json:"contract_version"`
	Publisher       string           `json:"publisher"`
	PublisherKeyID  string           `json:"publisher_key_id"`
	Licence         string           `json:"licence"`
	Entitlement     string           `json:"entitlement"`
	Tier            string           `json:"tier"`
	DefaultEnabled  bool             `json:"default_enabled"`
	Summary         string           `json:"summary,omitempty"`
	DocsURL         string           `json:"docs_url,omitempty"`
	Nav             *Nav             `json:"nav"`
	Slots           []string         `json:"slots,omitempty"`
	UIDigest        string           `json:"ui_digest,omitempty"`
	SettingsSchema  map[string]any   `json:"settings_schema,omitempty"`
	Permissions     []string         `json:"permissions"`
	ExtensionPoints []ExtensionPoint `json:"extension_points,omitempty"`
	Routes          []Route          `json:"routes,omitempty"`
	Jobs            []Job            `json:"jobs,omitempty"`
	Content         *Content         `json:"content"`
	Resources       *Resources       `json:"resources,omitempty"`
	Network         *Network         `json:"network,omitempty"`
	OpenAPIPath     string           `json:"openapi_path,omitempty"`
}
