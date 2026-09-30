package code

// This file defines the shared vocabulary of the Code facet's overwrite
// approval gate: the one Documents collection and the one approval scope that
// cover every kind of overwrite, the target a proposal names, and the
// write/read pair that puts a resource payload into a proposal body and gets
// it back out.
//
// It deliberately imports only the generated wire types, protojson and the
// standard library. It must not import the approval package or the host
// package: this package is the format/model layer, and dragging governance or
// host code into everything that imports it would invert that layering. A
// pack composes approval.Kind{Collection: code.OverwriteCollection,
// ApproveScope: code.OverwriteApproveScope} at its own call site.
//
// Nothing here reads or writes a proposal's "status" key. The proposal
// status belongs to the approval package alone (approval.ProposeBody sets it
// to pending and approval.Approve/Reject change it); a payload helper that
// touched it could hand a caller a proposal that is born approved.

import (
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// OverwriteCollection is the single Documents collection holding every
// overwrite proposal, whatever resource type it targets (D-06). The type is
// carried inside the proposal body's target, never by a separate collection.
const OverwriteCollection = "code-overwrites"

// OverwriteApproveScope is the single approval scope required to decide any
// overwrite proposal (D-06, D-07). It is a route access scope handed to
// approval.Approve through an approval.Kind; it is not a manifest permission
// and no manifest vocabulary mentions it.
const OverwriteApproveScope = "code:approve"

// The five overwrite resource kinds an OverwriteTarget can name.
const (
	OverwriteResourceEnvironment      = "environment"
	OverwriteResourceSettings         = "settings"
	OverwriteResourcePuppetfileModule = "puppetfile_module"
	OverwriteResourceHieraLevel       = "hiera_level"
	OverwriteResourceHieraDataKey     = "hiera_data_key"
)

// ErrOverwriteBodyInvalid is returned (wrapped) when a proposal body cannot
// be read as an overwrite proposal: the target is absent or malformed, names
// an unknown resource kind, or the payload is absent or does not decode.
var ErrOverwriteBodyInvalid = errors.New("code: overwrite proposal body is invalid")

// OverwriteTarget names exactly which piece of existing content an overwrite
// proposal covers. Two targets are the same target only when all five fields
// are equal; the gate compares them by plain struct equality with no prefix,
// substring or case-insensitive matching anywhere.
//
// Environment is always the environment whose content is overwritten. Resource
// is one of the OverwriteResource* constants. Name carries the module name,
// the Hiera level name or the Hiera data key, depending on Resource, and is
// empty for a whole-environment or settings overwrite. Path carries the Hiera
// data file's relative path and is empty otherwise. Source carries the
// source environment for an environment duplicate and is empty otherwise.
type OverwriteTarget struct {
	Environment string
	Resource    string
	Name        string
	Path        string
	Source      string
}

// Body keys. They are unexported and referenced by name, never repeated as
// literals, so the write side and the read side cannot drift apart.
const (
	overwriteKeyTarget      = "target"
	overwriteKeyPayload     = "payload"
	overwriteKeyEnvironment = "environment"
	overwriteKeyResource    = "resource"
	overwriteKeyName        = "name"
	overwriteKeyPath        = "path"
	overwriteKeySource      = "source"
)

func validOverwriteResource(r string) bool {
	switch r {
	case OverwriteResourceEnvironment,
		OverwriteResourceSettings,
		OverwriteResourcePuppetfileModule,
		OverwriteResourceHieraLevel,
		OverwriteResourceHieraDataKey:
		return true
	}
	return false
}

// overwriteTargetMap renders t as the plain map stored under the body's
// target key. Every field is written, including empty ones, so the stored
// shape is the same for every resource kind.
func overwriteTargetMap(t OverwriteTarget) map[string]any {
	return map[string]any{
		overwriteKeyEnvironment: t.Environment,
		overwriteKeyResource:    t.Resource,
		overwriteKeyName:        t.Name,
		overwriteKeyPath:        t.Path,
		overwriteKeySource:      t.Source,
	}
}

// ParseOverwriteTarget reads the target object out of a decoded proposal
// body. It returns an error wrapping ErrOverwriteBodyInvalid when the target
// key is absent, is not an object, has a field that is not a string, has an
// empty environment, or carries a resource outside the five constants. It
// never guesses: a target it cannot read exactly is a target that covers
// nothing.
func ParseOverwriteTarget(body map[string]any) (OverwriteTarget, error) {
	raw, ok := body[overwriteKeyTarget]
	if !ok {
		return OverwriteTarget{}, fmt.Errorf("%w: no %q key", ErrOverwriteBodyInvalid, overwriteKeyTarget)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return OverwriteTarget{}, fmt.Errorf("%w: %q is not an object", ErrOverwriteBodyInvalid, overwriteKeyTarget)
	}

	field := func(key string) (string, error) {
		v, present := m[key]
		if !present {
			return "", nil
		}
		s, isStr := v.(string)
		if !isStr {
			return "", fmt.Errorf("%w: target %q is not a string", ErrOverwriteBodyInvalid, key)
		}
		return s, nil
	}

	var t OverwriteTarget
	var err error
	if t.Environment, err = field(overwriteKeyEnvironment); err != nil {
		return OverwriteTarget{}, err
	}
	if t.Resource, err = field(overwriteKeyResource); err != nil {
		return OverwriteTarget{}, err
	}
	if t.Name, err = field(overwriteKeyName); err != nil {
		return OverwriteTarget{}, err
	}
	if t.Path, err = field(overwriteKeyPath); err != nil {
		return OverwriteTarget{}, err
	}
	if t.Source, err = field(overwriteKeySource); err != nil {
		return OverwriteTarget{}, err
	}
	if t.Environment == "" {
		return OverwriteTarget{}, fmt.Errorf("%w: target has no environment", ErrOverwriteBodyInvalid)
	}
	if !validOverwriteResource(t.Resource) {
		return OverwriteTarget{}, fmt.Errorf("%w: target resource %q is not one of the five overwrite resource kinds", ErrOverwriteBodyInvalid, t.Resource)
	}
	return t, nil
}

// overwriteBody assembles the proposal body for a target and an already
// rendered payload map.
func overwriteBody(t OverwriteTarget, payload map[string]any) map[string]any {
	return map[string]any{
		overwriteKeyTarget:  overwriteTargetMap(t),
		overwriteKeyPayload: payload,
	}
}

// OverwriteBodyForPuppetfileModule builds the body of a proposal to replace
// the Puppetfile module m in environment env. The target's Resource is
// OverwriteResourcePuppetfileModule and its Name is the module's name; the
// payload is protojson's rendering of m, frozen at propose time. The result
// carries no status key.
func OverwriteBodyForPuppetfileModule(env string, m *hostv1.PuppetfileModule) (map[string]any, error) {
	if m == nil || m.GetName() == "" {
		return nil, fmt.Errorf("%w: a module with a name is required", ErrOverwriteBodyInvalid)
	}
	data, err := protojson.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: rendering module payload: %v", ErrOverwriteBodyInvalid, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("%w: decoding rendered module payload: %v", ErrOverwriteBodyInvalid, err)
	}
	return overwriteBody(OverwriteTarget{
		Environment: env,
		Resource:    OverwriteResourcePuppetfileModule,
		Name:        m.GetName(),
	}, payload), nil
}

// OverwritePayloadPuppetfileModule is the exact inverse of
// OverwriteBodyForPuppetfileModule: it decodes the frozen module out of a
// proposal body. It refuses a body whose target is not a puppetfile_module
// target, so a payload can never be decoded as the wrong resource type.
func OverwritePayloadPuppetfileModule(body map[string]any) (*hostv1.PuppetfileModule, error) {
	t, err := ParseOverwriteTarget(body)
	if err != nil {
		return nil, err
	}
	if t.Resource != OverwriteResourcePuppetfileModule {
		return nil, fmt.Errorf("%w: target resource is %q, not %q", ErrOverwriteBodyInvalid, t.Resource, OverwriteResourcePuppetfileModule)
	}
	raw, ok := body[overwriteKeyPayload]
	if !ok {
		return nil, fmt.Errorf("%w: no %q key", ErrOverwriteBodyInvalid, overwriteKeyPayload)
	}
	if _, isObj := raw.(map[string]any); !isObj {
		return nil, fmt.Errorf("%w: %q is not an object", ErrOverwriteBodyInvalid, overwriteKeyPayload)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: re-encoding module payload: %v", ErrOverwriteBodyInvalid, err)
	}
	m := &hostv1.PuppetfileModule{}
	if err := protojson.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("%w: decoding module payload: %v", ErrOverwriteBodyInvalid, err)
	}
	return m, nil
}

// overwritePayloadObject returns the payload object of body after checking
// that the body's target names the wanted resource kind, so a payload can
// never be decoded as the wrong resource type.
func overwritePayloadObject(body map[string]any, wantResource string) (OverwriteTarget, map[string]any, error) {
	t, err := ParseOverwriteTarget(body)
	if err != nil {
		return OverwriteTarget{}, nil, err
	}
	if t.Resource != wantResource {
		return OverwriteTarget{}, nil, fmt.Errorf("%w: target resource is %q, not %q", ErrOverwriteBodyInvalid, t.Resource, wantResource)
	}
	raw, ok := body[overwriteKeyPayload]
	if !ok {
		return OverwriteTarget{}, nil, fmt.Errorf("%w: no %q key", ErrOverwriteBodyInvalid, overwriteKeyPayload)
	}
	obj, isObj := raw.(map[string]any)
	if !isObj {
		return OverwriteTarget{}, nil, fmt.Errorf("%w: %q is not an object", ErrOverwriteBodyInvalid, overwriteKeyPayload)
	}
	return t, obj, nil
}

// protoToMap renders m through protojson into a plain map, the shape a
// proposal body stores.
func protoToMap(m proto.Message) (map[string]any, error) {
	data, err := protojson.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// mapToProto is protoToMap's inverse: it decodes a plain map through
// protojson into dst.
func mapToProto(obj map[string]any, dst proto.Message) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(data, dst)
}

// OverwriteBodyForSettings builds the body of a proposal to replace an
// environment's whole settings record. The target's Environment is the record's
// own Environment and its Resource is OverwriteResourceSettings; Name, Path and
// Source are empty because settings has no per-item identity, which is exactly
// why the gate covers every settings write (D-03). The payload is protojson's
// rendering of a clone with Environment cleared, so the environment name is not
// stored twice, and protojson keeps an unset optional field unset. The result
// carries no status key.
func OverwriteBodyForSettings(s *hostv1.EnvironmentSettings) (map[string]any, error) {
	if s == nil || s.GetEnvironment() == "" {
		return nil, fmt.Errorf("%w: settings with an environment are required", ErrOverwriteBodyInvalid)
	}
	clone := proto.Clone(s).(*hostv1.EnvironmentSettings)
	clone.Environment = ""
	payload, err := protoToMap(clone)
	if err != nil {
		return nil, fmt.Errorf("%w: rendering settings payload: %v", ErrOverwriteBodyInvalid, err)
	}
	return overwriteBody(OverwriteTarget{
		Environment: s.GetEnvironment(),
		Resource:    OverwriteResourceSettings,
	}, payload), nil
}

// OverwritePayloadSettings is the exact inverse of OverwriteBodyForSettings.
// It decodes through protojson so an unset optional field stays unset rather
// than becoming an empty string. The returned record's Environment is the
// target's Environment.
func OverwritePayloadSettings(body map[string]any) (*hostv1.EnvironmentSettings, error) {
	t, obj, err := overwritePayloadObject(body, OverwriteResourceSettings)
	if err != nil {
		return nil, err
	}
	s := &hostv1.EnvironmentSettings{}
	if err := mapToProto(obj, s); err != nil {
		return nil, fmt.Errorf("%w: decoding settings payload: %v", ErrOverwriteBodyInvalid, err)
	}
	s.Environment = t.Environment
	return s, nil
}

// OverwriteBodyForEnvironmentDuplicate builds the body of a proposal to
// duplicate environment sourceName over the existing environment targetName.
// The target's Environment is targetName (the environment whose content is
// overwritten), its Resource is OverwriteResourceEnvironment and its Source is
// sourceName; Name and Path are empty.
//
// Unlike every other builder here it writes no payload key. What the approver
// authorizes is the operation, replace the target with a copy of the source, not
// a frozen document set, so the source is read again when the proposal is
// applied. That asymmetry is deliberate. The result carries no status key.
func OverwriteBodyForEnvironmentDuplicate(sourceName, targetName string) (map[string]any, error) {
	if sourceName == "" || targetName == "" {
		return nil, fmt.Errorf("%w: a source and a target environment are required", ErrOverwriteBodyInvalid)
	}
	return map[string]any{
		overwriteKeyTarget: overwriteTargetMap(OverwriteTarget{
			Environment: targetName,
			Resource:    OverwriteResourceEnvironment,
			Source:      sourceName,
		}),
	}, nil
}

// Payload keys inside a Hiera level proposal's payload object.
const (
	overwriteKeyLevel  = "level"
	overwriteKeyIndex  = "index"
	overwriteKeyInsert = "insert"
)

// OverwriteBodyForHieraLevel builds the body of a proposal to replace the
// Hiera level lvl in environment env. The target's Resource is
// OverwriteResourceHieraLevel and its Name is the level's name. The payload
// carries protojson's rendering of the level plus the index and the insert
// flag of the write the proposer attempted, so the Apply RPC reproduces the
// write the approver saw. A level carrying lookup_options is refused because
// lookup_options is read-only and no write may carry it. The result carries no
// status key.
func OverwriteBodyForHieraLevel(env string, lvl *hostv1.HieraLevel, index int32, insert bool) (map[string]any, error) {
	if env == "" {
		return nil, fmt.Errorf("%w: an environment is required", ErrOverwriteBodyInvalid)
	}
	if lvl == nil || lvl.GetName() == "" {
		return nil, fmt.Errorf("%w: a level with a name is required", ErrOverwriteBodyInvalid)
	}
	if len(lvl.GetLookupOptions()) > 0 {
		return nil, fmt.Errorf("%w: lookup_options is read-only and cannot be part of a write", ErrOverwriteBodyInvalid)
	}
	rendered, err := protoToMap(lvl)
	if err != nil {
		return nil, fmt.Errorf("%w: rendering level payload: %v", ErrOverwriteBodyInvalid, err)
	}
	return overwriteBody(OverwriteTarget{
		Environment: env,
		Resource:    OverwriteResourceHieraLevel,
		Name:        lvl.GetName(),
	}, map[string]any{
		overwriteKeyLevel:  rendered,
		overwriteKeyIndex:  float64(index),
		overwriteKeyInsert: insert,
	}), nil
}

// OverwritePayloadHieraLevel is the exact inverse of OverwriteBodyForHieraLevel:
// it returns the frozen level, index and insert flag. The index is read as a
// JSON number and must be integral and in the int32 range.
func OverwritePayloadHieraLevel(body map[string]any) (*hostv1.HieraLevel, int32, bool, error) {
	_, obj, err := overwritePayloadObject(body, OverwriteResourceHieraLevel)
	if err != nil {
		return nil, 0, false, err
	}
	rawLevel, ok := obj[overwriteKeyLevel].(map[string]any)
	if !ok {
		return nil, 0, false, fmt.Errorf("%w: payload has no %q object", ErrOverwriteBodyInvalid, overwriteKeyLevel)
	}
	lvl := &hostv1.HieraLevel{}
	if err := mapToProto(rawLevel, lvl); err != nil {
		return nil, 0, false, fmt.Errorf("%w: decoding level payload: %v", ErrOverwriteBodyInvalid, err)
	}
	var index int32
	switch n := obj[overwriteKeyIndex].(type) {
	case nil:
	case float64:
		if n != float64(int32(n)) {
			return nil, 0, false, fmt.Errorf("%w: payload index %v is not an int32", ErrOverwriteBodyInvalid, n)
		}
		index = int32(n)
	default:
		// structpb decoding only ever yields float64 for a number, so any
		// other type here is not a stored payload index.
		return nil, 0, false, fmt.Errorf("%w: payload index is not a number", ErrOverwriteBodyInvalid)
	}
	insert := false
	if raw, present := obj[overwriteKeyInsert]; present {
		b, isBool := raw.(bool)
		if !isBool {
			return nil, 0, false, fmt.Errorf("%w: payload insert is not a boolean", ErrOverwriteBodyInvalid)
		}
		insert = b
	}
	return lvl, index, insert, nil
}

// OverwriteBodyForHieraDataKey builds the body of a proposal to replace the
// value of key in the Hiera data file path of environment env. The target's
// Resource is OverwriteResourceHieraDataKey, its Name is the key and its Path
// is the file's relative path. Both participate in the five-field equality
// match, which is what makes an approval for a key in one file not cover the
// same key in another. The payload is protojson's rendering of the value,
// frozen at propose time. The result carries no status key.
func OverwriteBodyForHieraDataKey(env, path, key string, value *hostv1.Json) (map[string]any, error) {
	if env == "" || path == "" || key == "" {
		return nil, fmt.Errorf("%w: an environment, a data file path and a key are required", ErrOverwriteBodyInvalid)
	}
	if value == nil {
		value = &hostv1.Json{}
	}
	rendered, err := protoToMap(value)
	if err != nil {
		return nil, fmt.Errorf("%w: rendering data key payload: %v", ErrOverwriteBodyInvalid, err)
	}
	return overwriteBody(OverwriteTarget{
		Environment: env,
		Resource:    OverwriteResourceHieraDataKey,
		Name:        key,
		Path:        path,
	}, rendered), nil
}

// OverwritePayloadHieraDataKey is the exact inverse of
// OverwriteBodyForHieraDataKey: it decodes the frozen value.
func OverwritePayloadHieraDataKey(body map[string]any) (*hostv1.Json, error) {
	_, obj, err := overwritePayloadObject(body, OverwriteResourceHieraDataKey)
	if err != nil {
		return nil, err
	}
	v := &hostv1.Json{}
	if err := mapToProto(obj, v); err != nil {
		return nil, fmt.Errorf("%w: decoding data key payload: %v", ErrOverwriteBodyInvalid, err)
	}
	return v, nil
}
