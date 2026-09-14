package manifest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Finding is one validation failure. Code is stable; Fix tells a human or a
// code assistant what to change. Path is a JSON pointer-ish location.
type Finding struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

func (f Finding) String() string {
	return fmt.Sprintf("%s at %s: %s — fix: %s", f.Code, f.Path, f.Message, f.Fix)
}

var (
	reID      = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	reSemver  = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	reRoute   = regexp.MustCompile(`^/x/[a-z][a-z0-9_]{1,31}$`)
	reDigest  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	rePerm    = regexp.MustCompile(`^(documents:rw|secrets:rw|bolt:run|inventory:read|inventory:kind:[a-z][a-z0-9_]{1,31}|compliance:ingest|tokens:issue|forge:read|puppetdb:read|classification:read|code:read|activity:read|legacy:read:[a-z_]+)$`)
	reMethod  = regexp.MustCompile(`^[A-Z][A-Z-]{0,15}$`)
	rePath    = regexp.MustCompile(`^[^/][A-Za-z0-9_./{}-]*$`)
	reOpID    = regexp.MustCompile(`^[a-zA-Z][A-Za-z0-9]*$`)
	reScope   = regexp.MustCompile(`^[a-z][a-z0-9_:-]*$`)
	reJob     = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	reSlug    = regexp.MustCompile(`^[a-z0-9]+-[a-z0-9_]+$`)
	reClass   = regexp.MustCompile(`^[a-z][a-z0-9_]*(::[a-z][a-z0-9_]*)*$`)
	reCPU     = regexp.MustCompile(`^[0-9]+m?$`)
	reMem     = regexp.MustCompile(`^[0-9]+(Mi|Gi)$`)
	reVolName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	reHost    = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$|^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// Parse decodes a manifest strictly (unknown fields are findings, not ignored).
func Parse(raw []byte) (*Manifest, []Finding) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, []Finding{{Code: "manifest_unparseable", Path: "/", Message: err.Error(),
			Fix: "Make manifest.json valid JSON with only the fields in manifest/schema.json."}}
	}
	return &m, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Validate applies every rule the JSON Schema encodes plus the cross-field
// rules (page needs nav; any slot needs ui_digest; routes need openapi_path).
func Validate(m *Manifest) []Finding {
	var fs []Finding
	add := func(code, path, msg, fix string) { fs = append(fs, Finding{code, path, msg, fix}) }

	if !reID.MatchString(m.ID) {
		add("id_invalid", "/id", "id must match ^[a-z][a-z0-9_]{1,31}$", "Use a short lowercase id such as \"opentofu\".")
	}
	if strings.TrimSpace(m.Name) == "" || len(m.Name) > 64 {
		add("name_invalid", "/name", "name is required, 1–64 characters", "Set a human-readable name such as \"OpenTofu\".")
	}
	if !reSemver.MatchString(m.Version) {
		add("version_not_semver", "/version", "version must be semver", "Use MAJOR.MINOR.PATCH, e.g. \"1.0.0\".")
	}
	if m.ContractVersion != ContractVersion {
		add("contract_version_unsupported", "/contract_version", fmt.Sprintf("this SDK targets contract_version %d", ContractVersion),
			fmt.Sprintf("Set contract_version to %d, or upgrade the SDK.", ContractVersion))
	}
	if strings.TrimSpace(m.Publisher) == "" {
		add("publisher_missing", "/publisher", "publisher is required", "Set the publishing organisation, e.g. \"Perforce\" or your company.")
	}
	if len(m.PublisherKeyID) < 8 {
		add("publisher_key_missing", "/publisher_key_id", "publisher_key_id must identify the cosign key or identity", "Run `cosign public-key` on your signing key and use its fingerprint, or your keyless identity.")
	}
	if strings.TrimSpace(m.Licence) == "" {
		add("licence_missing", "/licence", "licence is required", "Use an SPDX id such as \"Apache-2.0\" or \"proprietary\".")
	}
	if !contains([]string{"none", "licence_key", "marketplace"}, m.Entitlement) {
		add("entitlement_invalid", "/entitlement", "entitlement must be none | licence_key | marketplace", "Use \"none\" unless the pack is paid.")
	}
	if !contains([]string{"core", "ent", "adv"}, m.Tier) {
		add("tier_invalid", "/tier", "tier must be core | ent | adv", "Use \"core\" for a free pack.")
	}
	if m.Permissions == nil {
		add("permissions_missing", "/permissions", "permissions is required (may be empty)", "Declare every facet the worker calls, e.g. [\"documents:rw\"].")
	}
	seenPerm := map[string]bool{}
	for i, p := range m.Permissions {
		path := fmt.Sprintf("/permissions/%d", i)
		if !rePerm.MatchString(p) {
			add("permission_unknown", path, "unknown permission "+p, "Use one of: "+strings.Join(Permissions, ", ")+", inventory:kind:<name>, legacy:read:<table>.")
		}
		if seenPerm[p] {
			add("permission_duplicate", path, "duplicate permission "+p, "Remove the duplicate.")
		}
		seenPerm[p] = true
	}

	// nav / slots / ui
	seenSlot := map[string]bool{}
	for i, s := range m.Slots {
		path := fmt.Sprintf("/slots/%d", i)
		if !contains(Slots, s) {
			add("slot_unknown", path, "unknown slot "+s, "Use one of: "+strings.Join(Slots, ", ")+".")
		}
		if seenSlot[s] {
			add("slot_duplicate", path, "duplicate slot "+s, "Remove the duplicate.")
		}
		seenSlot[s] = true
	}
	if seenSlot["page"] && m.Nav == nil {
		add("page_requires_nav", "/nav", "a page slot needs a nav entry", "Add nav: {group: <one of the seven groups>, label, route: \"/x/<id>\"}.")
	}
	if m.Nav != nil {
		if !contains(NavGroups, m.Nav.Group) {
			add("nav_group_unknown", "/nav/group", "nav.group must be one of the seven shipped groups", "Use one of: "+strings.Join(NavGroups, ", ")+". Packs add items only, never groups.")
		}
		if strings.TrimSpace(m.Nav.Label) == "" || len(m.Nav.Label) > 32 {
			add("nav_label_invalid", "/nav/label", "nav.label is required, 1–32 characters", "Set the menu label, e.g. \"OpenTofu\".")
		}
		if !reRoute.MatchString(m.Nav.Route) || (reID.MatchString(m.ID) && m.Nav.Route != "/x/"+m.ID) {
			add("nav_route_invalid", "/nav/route", "nav.route must be \"/x/<id>\"", fmt.Sprintf("Set nav.route to \"/x/%s\".", m.ID))
		}
		if !seenSlot["page"] {
			add("nav_without_page", "/nav", "nav is set but no page slot is declared", "Add \"page\" to slots, or remove nav for a headless pack.")
		}
	}
	if len(m.Slots) > 0 && !reDigest.MatchString(m.UIDigest) {
		add("ui_digest_missing", "/ui_digest", "packs with slots must carry the sha256 digest of /stagehand/ui", "Run `pack-build` — it computes and writes ui_digest; never hand-edit it.")
	}
	if len(m.Slots) == 0 && m.UIDigest != "" {
		add("ui_digest_without_slots", "/ui_digest", "ui_digest set on a headless pack", "Remove ui_digest or declare the slots the bundle provides.")
	}

	// extension points
	for i, ep := range m.ExtensionPoints {
		path := fmt.Sprintf("/extension_points/%d", i)
		if !contains(ExtensionPoints, ep.Point) {
			add("extension_point_unknown", path+"/point", "unknown extension point "+ep.Point, "Use one of: "+strings.Join(ExtensionPoints, ", ")+".")
		}
		if !reID.MatchString(ep.ID) {
			add("extension_point_id_invalid", path+"/id", "extension point id must match ^[a-z][a-z0-9_]{1,31}$", "Use a short lowercase id, e.g. \"pe_basic\".")
		}
	}

	// routes
	seenRoute := map[string]bool{}
	seenOp := map[string]bool{}
	for i, r := range m.Routes {
		path := fmt.Sprintf("/routes/%d", i)
		if !reMethod.MatchString(r.Method) {
			add("route_method_invalid", path+"/method", "method must be an uppercase HTTP token", "Use GET, POST, PUT, DELETE, or a custom token such as LOCK.")
		}
		if !rePath.MatchString(r.Path) {
			add("route_path_invalid", path+"/path", "path must be relative (no leading slash) and URL-safe", "Write \"state/{workspace}\" — the host mounts it under /api/v1/x/<id>/.")
		}
		key := r.Method + " " + r.Path
		if seenRoute[key] {
			add("route_duplicate", path, "duplicate route "+key, "Merge or rename the duplicate route.")
		}
		seenRoute[key] = true
		if !reOpID.MatchString(r.OperationID) {
			add("route_operation_id_invalid", path+"/operation_id", "operation_id must be an identifier", "Use lowerCamelCase, e.g. \"lockState\"; it must exist in your OpenAPI fragment.")
		}
		if seenOp[r.OperationID] {
			add("route_operation_id_duplicate", path+"/operation_id", "duplicate operation_id "+r.OperationID, "Each route needs its own operation_id.")
		}
		seenOp[r.OperationID] = true
		a := r.Access
		if a.Role != "" && !contains([]string{"global_admin", "deployer", "operator", "viewer"}, a.Role) {
			add("route_access_role_unknown", path+"/access/role", "unknown role "+a.Role, "Use global_admin, deployer, operator, viewer, or omit for any session.")
		}
		if a.Scope != "" && !reScope.MatchString(a.Scope) {
			add("route_access_scope_invalid", path+"/access/scope", "scope must be lowercase [a-z0-9_:-]", "Use e.g. \"state:rw\"; the host namespaces it as pack:<id>:state:rw.")
		}
		if a.Scope != "" && !seenPerm["tokens:issue"] {
			add("route_scope_requires_tokens_issue", path+"/access/scope", "a scoped route needs machine tokens the pack can issue", "Add \"tokens:issue\" to permissions.")
		}
		if a.Public && a.Role == "" && a.Scope == "" {
			add("route_public_without_own_auth", path+"/access", "public routes must carry their own auth", "Set access.scope (token) or access.role; public:true only marks that the route authenticates callers itself.")
		}
	}
	if len(m.Routes) > 0 && m.OpenAPIPath == "" {
		add("openapi_path_missing", "/openapi_path", "packs with routes must ship an OpenAPI fragment", "Set openapi_path to \"/stagehand/openapi.json\" and generate the fragment with pack-build.")
	}

	// jobs
	seenJob := map[string]bool{}
	for i, j := range m.Jobs {
		path := fmt.Sprintf("/jobs/%d", i)
		if !reJob.MatchString(j.Name) {
			add("job_name_invalid", path+"/name", "job name must match ^[a-z][a-z0-9_]{1,31}$", "Use e.g. \"prune\".")
		}
		if seenJob[j.Name] {
			add("job_duplicate", path+"/name", "duplicate job "+j.Name, "Job names are unique per pack.")
		}
		seenJob[j.Name] = true
		d, err := time.ParseDuration(j.Every)
		if err != nil || d < 30*time.Second {
			add("job_interval_invalid", path+"/every", "every must be a Go duration of at least 30s", "Use e.g. \"1h\" or \"5m\".")
		}
	}

	// content
	if m.Content != nil {
		if !reSlug.MatchString(m.Content.ForgeSlug) {
			add("content_slug_invalid", "/content/forge_slug", "forge_slug must be <author>-<module>", "Use the Forge slug, e.g. \"souldo-stagehand_opentofu\".")
		}
		if strings.TrimSpace(m.Content.Version) == "" {
			add("content_version_missing", "/content/version", "content.version (semver range) is required", "Use a range such as \">=1.0 <2\".")
		}
		if m.Content.RunnerClass != "" && !reClass.MatchString(m.Content.RunnerClass) {
			add("content_runner_class_invalid", "/content/runner_class", "runner_class must be a Puppet class name", "Use e.g. \"stagehand_opentofu::runner\".")
		}
		if !seenPerm["forge:read"] {
			add("content_requires_forge_read", "/permissions", "a pack with content needs forge:read to resolve it", "Add \"forge:read\" to permissions.")
		}
	}

	// resources / network
	if m.Resources != nil {
		if m.Resources.CPU != "" && !reCPU.MatchString(m.Resources.CPU) {
			add("resources_cpu_invalid", "/resources/cpu", "cpu must be millicores or whole cores", "Use e.g. \"500m\" or \"1\".")
		}
		if m.Resources.Memory != "" && !reMem.MatchString(m.Resources.Memory) {
			add("resources_memory_invalid", "/resources/memory", "memory must be <n>Mi or <n>Gi", "Use e.g. \"256Mi\".")
		}
		for i, v := range m.Resources.Volumes {
			path := fmt.Sprintf("/resources/volumes/%d", i)
			if !reVolName.MatchString(v.Name) {
				add("volume_name_invalid", path+"/name", "volume name must be lowercase kebab", "Use e.g. \"cache\".")
			}
			if !reMem.MatchString(v.Size) {
				add("volume_size_invalid", path+"/size", "size must be <n>Mi or <n>Gi", "Use e.g. \"1Gi\".")
			}
		}
	}
	if m.Network != nil {
		for i, h := range m.Network.Egress {
			if !reHost.MatchString(h) {
				add("network_egress_invalid", fmt.Sprintf("/network/egress/%d", i), "egress entries are hostnames", "List hostnames only, e.g. \"registry.opentofu.org\"; no schemes or ports.")
			}
		}
	}

	// settings schema — renderable subset
	if m.SettingsSchema != nil {
		fs = append(fs, validateSettingsSchema(m.SettingsSchema)...)
	}
	return fs
}

func validateSettingsSchema(s map[string]any) []Finding {
	var fs []Finding
	if t, _ := s["type"].(string); t != "object" {
		fs = append(fs, Finding{"settings_schema_not_object", "/settings_schema/type", "settings_schema.type must be \"object\"", "Wrap your fields in {type: \"object\", properties: {...}}."})
		return fs
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		fs = append(fs, Finding{"settings_schema_no_properties", "/settings_schema/properties", "settings_schema.properties is required", "Add at least one property, or remove settings_schema."})
		return fs
	}
	allowed := map[string]bool{"string": true, "integer": true, "number": true, "boolean": true, "array": true, "object": true}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		path := "/settings_schema/properties/" + name
		if !ok {
			fs = append(fs, Finding{"settings_field_invalid", path, "field must be an object", "Give the field a type."})
			continue
		}
		for k := range p {
			if k == "$ref" || k == "oneOf" || k == "anyOf" || k == "allOf" || k == "if" {
				fs = append(fs, Finding{"settings_schema_unsupported_keyword", path + "/" + k, "the host form renderer does not support " + k, "Flatten the field; see schema/json/settings-ui.schema.json."})
			}
		}
		t, _ := p["type"].(string)
		if !allowed[t] {
			fs = append(fs, Finding{"settings_field_type_unsupported", path + "/type", "unsupported field type " + fmt.Sprint(p["type"]), "Use string, integer, number, boolean, array (of strings) or a one-level object."})
		}
		if f, ok := p["format"].(string); ok && !contains([]string{"password", "uri", "hostname", "date", "duration"}, f) {
			fs = append(fs, Finding{"settings_field_format_unsupported", path + "/format", "unsupported format " + f, "Use password, uri, hostname, date or duration, or omit format."})
		}
	}
	return fs
}
