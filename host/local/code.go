package local

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// envCollection, puppetfileCollection, hieraHierarchyCollection and
// hieraDataCollection are the four Documents collections the Code facet
// owns, kept deliberately separate because they have different write
// cadences, different round-trip risk profiles, and different future
// approval-gate granularity. Each name is defined exactly once, here; no
// other production file in this repository may write the literal (test
// files that seed a fixture document directly through Documents are the
// sole exception).
const (
	envCollection            = "code-environments"
	puppetfileCollection     = "code-puppetfiles"
	hieraHierarchyCollection = "code-hiera-hierarchy"
	hieraDataCollection      = "code-hiera-data"
)

// reEnvName is Puppet's real deploy-time environment-name rule: an
// environment name becomes both a git branch and a real directory name at
// deploy time, so a name that fails this rule can never actually deploy —
// even though this milestone never deploys (D-04).
var reEnvName = regexp.MustCompile("^[a-z0-9_]+$")

// validateEnvName enforces D-04. It never lowercases, trims or otherwise
// normalizes name before checking: a name is either already valid or it is
// refused outright. An empty name fails the regex, so the same call covers
// the empty case.
func validateEnvName(name string) error {
	if !reEnvName.MatchString(name) {
		return status.Errorf(codes.InvalidArgument, "environment name %q must match ^[a-z0-9_]+$", name)
	}
	return nil
}

// codeServer is the real in-memory Code facet implementation. It
// deliberately carries no mutex of its own: every piece of Code-facet state
// lives in docs — the SAME documentsServer instance host.Host.Documents
// holds — so docs.mu is the one lock that orders all of it. A codeServer
// method that needs a read-modify-write sequence takes s.docs.mu once for
// its whole body and uses only the *Locked helpers inside; it must never
// call the documentsServer gRPC methods (Get/Put/Delete/List/Query) from
// inside that section, because those take the same lock and would
// deadlock.
type codeServer struct {
	hostv1.UnimplementedCodeServer
	packID string
	docs   *documentsServer
	// secrets is the SAME secretsServer instance backing host.Host.Secrets,
	// held so a later import RPC can resolve a sealed git credential by name
	// the way forgeServer resolves a private source's secret. Nothing reads it
	// yet; it is wired now so newCodeServer's signature changes once.
	secrets *secretsServer
	// git is the injectable git seam the import RPCs fetch through. A nil
	// client given to newCodeServer is replaced by DefaultGitClient().
	git GitClient
	// importLimits and inspectTimeout are the import ceilings, copied from the
	// named constants in the constructor so a test can shrink the field the way
	// newForgeServer's callTimeout is shrunk. importLimits is read-only after
	// construction.
	importLimits   code.ImportLimits
	inspectTimeout time.Duration
}

// newCodeServer wires a codeServer. A nil git defaults to the real
// system-git adapter, the same way newForgeServer defaults a nil ForgeClient;
// tests and pack examples inject a fixture with WithGitClient instead.
func newCodeServer(packID string, docs *documentsServer, secrets *secretsServer, git GitClient) *codeServer {
	if git == nil {
		git = DefaultGitClient()
	}
	return &codeServer{
		packID:         packID,
		docs:           docs,
		secrets:        secrets,
		git:            git,
		importLimits:   code.DefaultImportLimits(),
		inspectTimeout: importInspectTimeout,
	}
}

// envDocBody builds the code-environments collection's stored body for a
// freshly created environment: {"name": "<name>"}. No "settings" key is
// written here — per D-06, a freshly created environment has written no
// settings, and settings stays absent until a pack writes one.
func envDocBody(name string) (*hostv1.Json, error) {
	st, err := structpb.NewStruct(map[string]any{"name": name})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "building environment document body: %v", err)
	}
	return &hostv1.Json{Value: st}, nil
}

// environmentFromDoc reads a stored code-environments Document back into an
// *hostv1.Environment, copying CreatedAt/UpdatedAt straight from the
// Document. Callers that hand the result outward must still clone it via
// cloneEnvironment — this function does not.
func environmentFromDoc(d *hostv1.Document) (*hostv1.Environment, error) {
	if d == nil || d.Body == nil || d.Body.Value == nil {
		return nil, status.Error(codes.Internal, "environment document has no body")
	}
	name, ok := d.Body.Value.AsMap()["name"].(string)
	if !ok {
		return nil, status.Errorf(codes.Internal, "environment document %q has no string name field", d.DocId)
	}
	return &hostv1.Environment{
		Name:      name,
		CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt,
	}, nil
}

// cloneEnvironment returns a deep copy so a caller mutating the returned
// environment can never reach into the facet's internal state. Every read
// path returns through this, never a pointer into stored state.
func cloneEnvironment(e *hostv1.Environment) *hostv1.Environment {
	return proto.Clone(e).(*hostv1.Environment)
}

// hieraDataKey and hieraDataEnvAndPath are the matched constructor/
// deconstructor pair for the code-hiera-data collection's composite doc id
// <environment>/<relative-path>. The split is unambiguous only because
// validateEnvName restricts an environment name to the character class
// ^[a-z0-9_]+$, which contains no "/" — so the first "/" always ends the
// environment segment. This is the only place in this repository where
// that separator appears as a literal.
func hieraDataKey(env, path string) string {
	return env + "/" + path
}

func hieraDataEnvAndPath(docID string) (env, path string, ok bool) {
	for i := 0; i < len(docID); i++ {
		if docID[i] == '/' {
			return docID[:i], docID[i+1:], true
		}
	}
	return "", "", false
}

// CreateEnvironment validates name against D-04, then writes a new
// code-environments document under a single s.docs.mu acquisition using the
// create-only putLocked path — a name collision returns codes.AlreadyExists
// (D-05) and leaves the existing document untouched.
func (s *codeServer) CreateEnvironment(ctx context.Context, req *hostv1.CreateEnvironmentRequest) (*hostv1.Environment, error) {
	if err := validateEnvName(req.Name); err != nil {
		return nil, err
	}
	body, err := envDocBody(req.Name)
	if err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, err := s.docs.putLocked(envCollection, req.Name, body, true); err != nil {
		return nil, err
	}
	doc, ok := s.docs.getLocked(envCollection, req.Name)
	if !ok {
		return nil, status.Errorf(codes.Internal, "environment %q vanished immediately after create", req.Name)
	}
	env, err := environmentFromDoc(doc)
	if err != nil {
		return nil, err
	}
	return cloneEnvironment(env), nil
}

// GetEnvironment looks a name up by exact byte equality — no case folding,
// no Unicode normalization, no trimming. An empty name returns
// InvalidArgument rather than NotFound, because an empty name can never
// name an environment and the two errors mean different things to a
// caller.
func (s *codeServer) GetEnvironment(ctx context.Context, req *hostv1.GetEnvironmentRequest) (*hostv1.Environment, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "environment name is required")
	}
	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	doc, ok := s.docs.getLocked(envCollection, req.Name)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Name)
	}
	env, err := environmentFromDoc(doc)
	if err != nil {
		return nil, err
	}
	return cloneEnvironment(env), nil
}

// ListEnvironments returns every environment ascending by name (the
// ordering idsLocked's sort.Strings already provides), paginated through
// the shared pageBounds helper — the exact same cursor-bounds contract
// Inventory's ListNodes uses. An empty store returns zero environments, an
// empty NextCursor and a nil error.
func (s *codeServer) ListEnvironments(ctx context.Context, req *hostv1.ListEnvironmentsRequest) (*hostv1.ListEnvironmentsResponse, error) {
	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	ids := s.docs.idsLocked(envCollection)
	start, limit, err := pageBounds(req.Page, len(ids))
	if err != nil {
		return nil, err
	}

	envs := make([]*hostv1.Environment, 0, limit)
	end := start
	for end < len(ids) && len(envs) < limit {
		doc, ok := s.docs.getLocked(envCollection, ids[end])
		if ok {
			env, err := environmentFromDoc(doc)
			if err != nil {
				return nil, err
			}
			envs = append(envs, cloneEnvironment(env))
		}
		end++
	}
	pageInfo := &hostv1.PageInfo{}
	if end < len(ids) {
		pageInfo.NextCursor = strconv.Itoa(end)
	}
	return &hostv1.ListEnvironmentsResponse{Environments: envs, Page: pageInfo}, nil
}

// docRef names one document by its collection and doc id. It is the unit
// envOwnedDocsLocked returns and the unit the rename/delete/duplicate
// two-pass shape plans over.
type docRef struct {
	collection string
	docID      string
}

// envOwnedDocsLocked resolves every document environment env owns, per
// <ownership_contract>: the single document whose doc id equals env in each
// of code-environments, code-puppetfiles and code-hiera-hierarchy when
// present, plus every code-hiera-data document whose composite doc id's
// environment segment — decided by hieraDataEnvAndPath's first-"/" split,
// never by a plain strings.HasPrefix — equals env. This is the single place
// environment-to-document ownership is decided; RenameEnvironment,
// DeleteEnvironment and DuplicateEnvironment all read it here instead of
// re-deriving it. The caller must already hold s.docs.mu.
func (s *codeServer) envOwnedDocsLocked(env string) []docRef {
	var refs []docRef
	for _, coll := range []string{envCollection, puppetfileCollection, hieraHierarchyCollection} {
		if _, ok := s.docs.getLocked(coll, env); ok {
			refs = append(refs, docRef{collection: coll, docID: env})
		}
	}
	for _, id := range s.docs.idsLocked(hieraDataCollection) {
		if docEnv, _, ok := hieraDataEnvAndPath(id); ok && docEnv == env {
			refs = append(refs, docRef{collection: hieraDataCollection, docID: id})
		}
	}
	return refs
}

// targetInUseLocked reports whether any document already exists under name
// in any of the four Code collections — the collision check
// RenameEnvironment and DuplicateEnvironment both run in pass 1, before
// planning a single write, per D-05. The caller must already hold
// s.docs.mu.
func (s *codeServer) targetInUseLocked(name string) bool {
	for _, coll := range []string{envCollection, puppetfileCollection, hieraHierarchyCollection} {
		if _, ok := s.docs.getLocked(coll, name); ok {
			return true
		}
	}
	for _, id := range s.docs.idsLocked(hieraDataCollection) {
		if docEnv, _, ok := hieraDataEnvAndPath(id); ok && docEnv == name {
			return true
		}
	}
	return false
}

// plannedDocWrite is one document RenameEnvironment or DuplicateEnvironment
// plans to write under a target environment name, built in pass 1 and
// applied unconditionally in pass 2 (<ownership_contract>'s two-pass
// decide-then-apply shape) — no error is reachable in pass 2 because pass 1
// already proved the target absent in every collection.
type plannedDocWrite struct {
	collection string
	docID      string
	body       *hostv1.Json
}

// planRekeyedWritesLocked reads every document refs names and rebuilds each
// one's body and doc id as it would look under targetEnv: the identity
// document's "name" field is rewritten and its doc id becomes targetEnv,
// the Puppetfile and hierarchy documents keep their bodies unchanged and
// move to doc id targetEnv, and each code-hiera-data document is re-keyed
// through hieraDataKey(targetEnv, path) — the matched constructor for the
// composite key hieraDataEnvAndPath deconstructs. Every body is
// proto.Clone'd so the rekeyed write and the source document never share a
// *hostv1.Json value (T-06-25: a later edit to one must never change the
// other). The caller must already hold s.docs.mu.
func (s *codeServer) planRekeyedWritesLocked(refs []docRef, targetEnv string) ([]plannedDocWrite, error) {
	writes := make([]plannedDocWrite, 0, len(refs))
	for _, ref := range refs {
		doc, ok := s.docs.getLocked(ref.collection, ref.docID)
		if !ok {
			return nil, status.Errorf(codes.Internal, "document %s/%s vanished mid-operation", ref.collection, ref.docID)
		}
		body := proto.Clone(doc.Body).(*hostv1.Json)
		newDocID := targetEnv

		switch ref.collection {
		case envCollection:
			m := body.Value.AsMap()
			m["name"] = targetEnv
			st, err := structpb.NewStruct(m)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "rebuilding identity document for %q: %v", targetEnv, err)
			}
			body = &hostv1.Json{Value: st}
		case hieraDataCollection:
			_, path, ok := hieraDataEnvAndPath(ref.docID)
			if !ok {
				return nil, status.Errorf(codes.Internal, "hiera data doc id %q has no environment separator", ref.docID)
			}
			newDocID = hieraDataKey(targetEnv, path)
		}
		writes = append(writes, plannedDocWrite{collection: ref.collection, docID: newDocID, body: body})
	}
	return writes, nil
}

// RenameEnvironment validates both names against D-04 before taking any
// lock, then moves every document req.Name owns to req.NewName under a
// single s.docs.mu acquisition: pass 1 confirms the source exists and the
// target is absent everywhere (D-05), pass 2 writes every rekeyed document
// with the create-only helper and deletes every source document. Neither
// pass calls a documentsServer gRPC method — only the lock-free *Locked
// helpers — so the whole move is indivisible.
func (s *codeServer) RenameEnvironment(ctx context.Context, req *hostv1.RenameEnvironmentRequest) (*hostv1.Environment, error) {
	if err := validateEnvName(req.Name); err != nil {
		return nil, err
	}
	if err := validateEnvName(req.NewName); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Name); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Name)
	}
	if s.targetInUseLocked(req.NewName) {
		return nil, status.Errorf(codes.AlreadyExists, "environment %q already exists", req.NewName)
	}

	refs := s.envOwnedDocsLocked(req.Name)
	writes, err := s.planRekeyedWritesLocked(refs, req.NewName)
	if err != nil {
		return nil, err
	}

	for _, w := range writes {
		if _, err := s.docs.putLocked(w.collection, w.docID, w.body, true); err != nil {
			return nil, status.Errorf(codes.Internal, "rename: writing %s/%s: %v", w.collection, w.docID, err)
		}
	}
	for _, ref := range refs {
		s.docs.deleteLocked(ref.collection, ref.docID)
	}

	doc, ok := s.docs.getLocked(envCollection, req.NewName)
	if !ok {
		return nil, status.Errorf(codes.Internal, "renamed environment %q vanished immediately", req.NewName)
	}
	env, err := environmentFromDoc(doc)
	if err != nil {
		return nil, err
	}
	return cloneEnvironment(env), nil
}

// DeleteEnvironment removes every document req.Name owns — its identity
// document plus its Puppetfile, hierarchy and Hiera data documents when
// present — under a single s.docs.mu acquisition. An unknown name returns
// codes.NotFound and deletes nothing.
func (s *codeServer) DeleteEnvironment(ctx context.Context, req *hostv1.DeleteEnvironmentRequest) (*emptypb.Empty, error) {
	if err := validateEnvName(req.Name); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Name); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Name)
	}
	for _, ref := range s.envOwnedDocsLocked(req.Name) {
		s.docs.deleteLocked(ref.collection, ref.docID)
	}
	return &emptypb.Empty{}, nil
}

// settingsFromDoc reads the "settings" sub-object out of a stored
// code-environments Document body into a *hostv1.EnvironmentSettings. When
// the key is absent, or present as an empty object, every setting field
// stays unset — D-06's absent-stays-absent guarantee. The round trip goes
// through protojson rather than hand-mapped fields so proto3 explicit
// presence (optional) is preserved in both directions: protojson omits an
// unset field from its JSON entirely and leaves it unset on decode.
func settingsFromDoc(d *hostv1.Document, env string) (*hostv1.EnvironmentSettings, error) {
	settings := &hostv1.EnvironmentSettings{}
	if d != nil && d.Body != nil && d.Body.Value != nil {
		if raw, ok := d.Body.Value.AsMap()["settings"]; ok {
			if settingsMap, ok := raw.(map[string]any); ok && len(settingsMap) > 0 {
				data, err := json.Marshal(settingsMap)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "marshaling stored settings for %q: %v", env, err)
				}
				if err := protojson.Unmarshal(data, settings); err != nil {
					return nil, status.Errorf(codes.Internal, "decoding stored settings for %q: %v", env, err)
				}
			}
		}
	}
	settings.Environment = env
	return settings, nil
}

// settingsIntoBody produces the updated code-environments Document body for
// a PutEnvironmentSettings write: existing's "name" field is preserved
// unchanged, and its "settings" sub-object is wholly replaced (never
// merged) with protojson's rendering of s, with Environment cleared first —
// the environment name lives in the document's key and its "name" field, so
// storing a third copy inside "settings" would be a value that can disagree
// with itself.
func settingsIntoBody(existing *hostv1.Json, s *hostv1.EnvironmentSettings) (*hostv1.Json, error) {
	var name string
	if existing != nil && existing.Value != nil {
		if n, ok := existing.Value.AsMap()["name"].(string); ok {
			name = n
		}
	}

	clone := proto.Clone(s).(*hostv1.EnvironmentSettings)
	clone.Environment = ""
	data, err := protojson.Marshal(clone)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshaling settings: %v", err)
	}
	var settingsMap map[string]any
	if err := json.Unmarshal(data, &settingsMap); err != nil {
		return nil, status.Errorf(codes.Internal, "decoding settings json: %v", err)
	}

	st, err := structpb.NewStruct(map[string]any{
		"name":     name,
		"settings": settingsMap,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "building environment document body: %v", err)
	}
	return &hostv1.Json{Value: st}, nil
}

// GetEnvironmentSettings validates the name, fetches the identity document
// or returns codes.NotFound, and returns settingsFromDoc's result — a
// never-configured environment's seven setting fields are all absent,
// never Puppet's documented defaults (D-06).
func (s *codeServer) GetEnvironmentSettings(ctx context.Context, req *hostv1.GetEnvironmentSettingsRequest) (*hostv1.EnvironmentSettings, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	doc, ok := s.docs.getLocked(envCollection, req.Environment)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}
	settings, err := settingsFromDoc(doc, req.Environment)
	if err != nil {
		return nil, err
	}
	return proto.Clone(settings).(*hostv1.EnvironmentSettings), nil
}

// checkSettingsRoundTrip renders settings to environment.conf text, re-parses
// it, and refuses with codes.InvalidArgument unless the two are proto.Equal
// with Environment cleared on both sides. PutEnvironmentSettings and
// ApplyEnvironmentSettings share it, so a record that could not be written
// directly cannot be written through an approval either.
func checkSettingsRoundTrip(settings *hostv1.EnvironmentSettings) error {
	rendered, err := code.RenderEnvConf(settings)
	if err != nil {
		if errors.Is(err, code.ErrEnvConfInvalid) {
			return status.Errorf(codes.InvalidArgument, "settings cannot be written to environment.conf: %v", err)
		}
		return status.Errorf(codes.Internal, "rendering settings: %v", err)
	}
	reparsed, err := code.ParseEnvConf(rendered)
	if err != nil {
		return status.Errorf(codes.Internal, "re-parsing rendered settings: %v", err)
	}
	want := proto.Clone(settings).(*hostv1.EnvironmentSettings)
	want.Environment = ""
	reparsed.Environment = ""
	if !proto.Equal(want, reparsed) {
		return status.Error(codes.InvalidArgument, "settings do not survive an environment.conf render/re-parse cycle unchanged")
	}
	return nil
}

// PutEnvironmentSettings replaces an environment's whole settings record.
// Before storing anything it renders req.Settings to environment.conf text
// via code.RenderEnvConf, re-parses that text via code.ParseEnvConf, and
// refuses with codes.InvalidArgument unless the two are proto.Equal (with
// Environment cleared on both sides, since environment.conf text carries no
// environment name) — a record that cannot survive that cycle unchanged
// would, the moment it reached a real environment.conf, forge an additional
// unauthored setting line (T-06-02). This also keeps environment_timeout
// opaque text: nothing in this path parses it as a number.
//
// Every call is then gated on an approved overwrite proposal (D-03): settings
// carries no per-field identity that would let a first write count as still
// creating, so once the environment exists this RPC never writes settings
// itself. It refuses with a structured error and the write happens only
// through ApplyEnvironmentSettings. The gate sits after the environment
// NotFound check and after the render/re-parse check above, so a malformed
// record is refused for being malformed, not for being ungated.
func (s *codeServer) PutEnvironmentSettings(ctx context.Context, req *hostv1.PutEnvironmentSettingsRequest) (*hostv1.EnvironmentSettings, error) {
	if req.Settings == nil {
		return nil, status.Error(codes.InvalidArgument, "settings is required")
	}
	if err := validateEnvName(req.Settings.Environment); err != nil {
		return nil, err
	}

	if err := checkSettingsRoundTrip(req.Settings); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Settings.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Settings.Environment)
	}

	target := code.OverwriteTarget{Environment: req.Settings.Environment, Resource: code.OverwriteResourceSettings}
	if id, approved := s.approvedOverwriteProposalLocked(target); approved {
		return nil, ErrCodeOverwriteApplyPending(id, applyEnvironmentSettingsRPC)
	}
	return nil, ErrCodeOverwriteRequiresApproval(target, applyEnvironmentSettingsRPC)
}

// DuplicateEnvironment validates both names against D-04 before taking any
// lock, then copies every document req.SourceName owns to req.TargetName
// under a single s.docs.mu acquisition: pass 1 confirms the source exists
// and the target is absent everywhere (D-05), pass 2 writes every rekeyed,
// deep-copied document with the create-only helper. No Documents gRPC
// method is ever called from inside the locked section, and the lock is
// never released between the two passes — nothing can appear at the target
// in between, so a concurrent reader observes either the complete copy or
// nothing (T-06-05).
//
// A target name already in use is not a bare AlreadyExists: it is an
// overwrite, refused with the structured FailedPrecondition error unless an
// approved proposal for this source and target exists, and materialized only
// by ApplyEnvironmentDuplicate. An unused target stays ungated create.
func (s *codeServer) DuplicateEnvironment(ctx context.Context, req *hostv1.DuplicateEnvironmentRequest) (*hostv1.Environment, error) {
	if err := validateEnvName(req.SourceName); err != nil {
		return nil, err
	}
	if err := validateEnvName(req.TargetName); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.SourceName); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.SourceName)
	}
	if s.targetInUseLocked(req.TargetName) {
		// Duplicating over a name already in use would replace whatever that
		// environment owns, so it is an overwrite: refused unless an approved
		// proposal covers this exact source-and-target pair (D-02, D-04).
		target := code.OverwriteTarget{
			Environment: req.TargetName,
			Resource:    code.OverwriteResourceEnvironment,
			Source:      req.SourceName,
		}
		if id, approved := s.approvedOverwriteProposalLocked(target); approved {
			return nil, ErrCodeOverwriteApplyPending(id, applyEnvironmentDuplicateRPC)
		}
		return nil, ErrCodeOverwriteRequiresApproval(target, applyEnvironmentDuplicateRPC)
	}

	refs := s.envOwnedDocsLocked(req.SourceName)
	writes, err := s.planRekeyedWritesLocked(refs, req.TargetName)
	if err != nil {
		return nil, err
	}

	for _, w := range writes {
		if _, err := s.docs.putLocked(w.collection, w.docID, w.body, true); err != nil {
			return nil, status.Errorf(codes.Internal, "duplicate: writing %s/%s: %v", w.collection, w.docID, err)
		}
	}

	doc, ok := s.docs.getLocked(envCollection, req.TargetName)
	if !ok {
		return nil, status.Errorf(codes.Internal, "duplicated environment %q vanished immediately", req.TargetName)
	}
	env, err := environmentFromDoc(doc)
	if err != nil {
		return nil, err
	}
	return cloneEnvironment(env), nil
}

// gatedCode wraps codeServer with the code:rw permission check every
// forwarded Code RPC requires, including the read-only ones and the Apply*
// RPCs — there is no narrower per-RPC permission and no read-only exemption.
// Every Code RPC is forwarded through this check rather than reaching the
// embedded UnimplementedCodeServer. Unlike Documents and Settings, which are
// always available, Code is a gated facet — the same posture Inventory ships.
//
// The three import RPCs (InspectImport, ProposeImport, ApplyImport) need more:
// code:rw first, then code:import, because an import clones an arbitrary git
// host and bulk-stages its content, which code:rw alone was never meant to
// grant. A forgotten forwarder would compile and return Unimplemented rather
// than PermissionDenied, because gatedCode embeds UnimplementedCodeServer;
// TestImport_Permissions is what proves each one exists.
type gatedCode struct {
	hostv1.UnimplementedCodeServer
	perms  map[string]bool
	packID string
	inner  *codeServer
}

func (g *gatedCode) check() error {
	if !g.perms["code:rw"] {
		return ErrPermissionDenied("code:rw")
	}
	return nil
}

// checkImport is the additional grant the three import RPCs need. It runs
// after check, so a host with neither permission is always told about code:rw.
func (g *gatedCode) checkImport() error {
	if !g.perms["code:import"] {
		return ErrPermissionDenied("code:import")
	}
	return nil
}

func (g *gatedCode) CreateEnvironment(ctx context.Context, req *hostv1.CreateEnvironmentRequest) (*hostv1.Environment, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.CreateEnvironment(ctx, req)
}

func (g *gatedCode) GetEnvironment(ctx context.Context, req *hostv1.GetEnvironmentRequest) (*hostv1.Environment, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.GetEnvironment(ctx, req)
}

func (g *gatedCode) ListEnvironments(ctx context.Context, req *hostv1.ListEnvironmentsRequest) (*hostv1.ListEnvironmentsResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ListEnvironments(ctx, req)
}

// The forwarders below cover the remaining Code RPCs. Each runs the code:rw
// gate first and then delegates to the matching codeServer method.

func (g *gatedCode) RenameEnvironment(ctx context.Context, req *hostv1.RenameEnvironmentRequest) (*hostv1.Environment, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.RenameEnvironment(ctx, req)
}

func (g *gatedCode) DeleteEnvironment(ctx context.Context, req *hostv1.DeleteEnvironmentRequest) (*emptypb.Empty, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.DeleteEnvironment(ctx, req)
}

func (g *gatedCode) DuplicateEnvironment(ctx context.Context, req *hostv1.DuplicateEnvironmentRequest) (*hostv1.Environment, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.DuplicateEnvironment(ctx, req)
}

func (g *gatedCode) GetEnvironmentSettings(ctx context.Context, req *hostv1.GetEnvironmentSettingsRequest) (*hostv1.EnvironmentSettings, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.GetEnvironmentSettings(ctx, req)
}

func (g *gatedCode) PutEnvironmentSettings(ctx context.Context, req *hostv1.PutEnvironmentSettingsRequest) (*hostv1.EnvironmentSettings, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.PutEnvironmentSettings(ctx, req)
}

func (g *gatedCode) ListPuppetfileModules(ctx context.Context, req *hostv1.ListPuppetfileModulesRequest) (*hostv1.ListPuppetfileModulesResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ListPuppetfileModules(ctx, req)
}

func (g *gatedCode) PutPuppetfileModule(ctx context.Context, req *hostv1.PutPuppetfileModuleRequest) (*hostv1.PuppetfileModule, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.PutPuppetfileModule(ctx, req)
}

// ApplyPuppetfileModuleOverwrite needs code:rw and nothing narrower, the same
// posture gatedInventory gives OnboardNode: the decision to allow the
// overwrite was recorded elsewhere, by a code:approve token holder, and this
// forwarder only checks the facet permission before delegating.
func (g *gatedCode) ApplyPuppetfileModuleOverwrite(ctx context.Context, req *hostv1.ApplyPuppetfileModuleOverwriteRequest) (*hostv1.PuppetfileModule, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ApplyPuppetfileModuleOverwrite(ctx, req)
}

// ApplyEnvironmentSettings and ApplyEnvironmentDuplicate need code:rw and
// nothing narrower, like every other Apply RPC.
func (g *gatedCode) ApplyEnvironmentSettings(ctx context.Context, req *hostv1.ApplyEnvironmentSettingsRequest) (*hostv1.EnvironmentSettings, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ApplyEnvironmentSettings(ctx, req)
}

func (g *gatedCode) ApplyEnvironmentDuplicate(ctx context.Context, req *hostv1.ApplyEnvironmentDuplicateRequest) (*hostv1.Environment, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ApplyEnvironmentDuplicate(ctx, req)
}

func (g *gatedCode) RemovePuppetfileModule(ctx context.Context, req *hostv1.RemovePuppetfileModuleRequest) (*emptypb.Empty, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.RemovePuppetfileModule(ctx, req)
}

func (g *gatedCode) SetModuledir(ctx context.Context, req *hostv1.SetModuledirRequest) (*hostv1.Puppetfile, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.SetModuledir(ctx, req)
}

func (g *gatedCode) RenderPuppetfile(ctx context.Context, req *hostv1.RenderPuppetfileRequest) (*hostv1.RenderedPuppetfile, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.RenderPuppetfile(ctx, req)
}

func (g *gatedCode) GetHieraHierarchy(ctx context.Context, req *hostv1.GetHieraHierarchyRequest) (*hostv1.HieraHierarchy, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.GetHieraHierarchy(ctx, req)
}

func (g *gatedCode) PutHieraLevel(ctx context.Context, req *hostv1.PutHieraLevelRequest) (*hostv1.PutHieraLevelResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.PutHieraLevel(ctx, req)
}

// ApplyHieraLevelOverwrite needs code:rw and nothing narrower, like every other
// Apply RPC.
func (g *gatedCode) ApplyHieraLevelOverwrite(ctx context.Context, req *hostv1.ApplyHieraLevelOverwriteRequest) (*hostv1.PutHieraLevelResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ApplyHieraLevelOverwrite(ctx, req)
}

func (g *gatedCode) RemoveHieraLevel(ctx context.Context, req *hostv1.RemoveHieraLevelRequest) (*hostv1.HieraHierarchy, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.RemoveHieraLevel(ctx, req)
}

func (g *gatedCode) ReorderHieraLevels(ctx context.Context, req *hostv1.ReorderHieraLevelsRequest) (*hostv1.HieraHierarchy, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ReorderHieraLevels(ctx, req)
}

func (g *gatedCode) ListHieraDataFiles(ctx context.Context, req *hostv1.ListHieraDataFilesRequest) (*hostv1.ListHieraDataFilesResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ListHieraDataFiles(ctx, req)
}

func (g *gatedCode) GetHieraDataFile(ctx context.Context, req *hostv1.GetHieraDataFileRequest) (*hostv1.HieraDataFile, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.GetHieraDataFile(ctx, req)
}

func (g *gatedCode) PutHieraDataKey(ctx context.Context, req *hostv1.PutHieraDataKeyRequest) (*hostv1.HieraDataFile, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.PutHieraDataKey(ctx, req)
}

// ApplyHieraDataKeyOverwrite needs code:rw and nothing narrower, like every
// other Apply RPC.
func (g *gatedCode) ApplyHieraDataKeyOverwrite(ctx context.Context, req *hostv1.ApplyHieraDataKeyOverwriteRequest) (*hostv1.HieraDataFile, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.ApplyHieraDataKeyOverwrite(ctx, req)
}

func (g *gatedCode) RemoveHieraDataKey(ctx context.Context, req *hostv1.RemoveHieraDataKeyRequest) (*hostv1.HieraDataFile, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.RemoveHieraDataKey(ctx, req)
}

func (g *gatedCode) DeleteHieraDataFile(ctx context.Context, req *hostv1.DeleteHieraDataFileRequest) (*emptypb.Empty, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	return g.inner.DeleteHieraDataFile(ctx, req)
}

// InspectImport, ProposeImport and ApplyImport require code:rw and then
// code:import (DQ-1). Neither check reaches the git client or Documents.
func (g *gatedCode) InspectImport(ctx context.Context, req *hostv1.InspectImportRequest) (*hostv1.InspectImportResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	if err := g.checkImport(); err != nil {
		return nil, err
	}
	return g.inner.InspectImport(ctx, req)
}

func (g *gatedCode) ProposeImport(ctx context.Context, req *hostv1.ProposeImportRequest) (*hostv1.ProposeImportResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	if err := g.checkImport(); err != nil {
		return nil, err
	}
	return g.inner.ProposeImport(ctx, req)
}

func (g *gatedCode) ApplyImport(ctx context.Context, req *hostv1.ApplyImportRequest) (*hostv1.ApplyImportResponse, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	if err := g.checkImport(); err != nil {
		return nil, err
	}
	return g.inner.ApplyImport(ctx, req)
}
