package local

import (
	"context"
	"regexp"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

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
}

func newCodeServer(packID string, docs *documentsServer) *codeServer {
	return &codeServer{packID: packID, docs: docs}
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

// gatedCode wraps codeServer with the code:rw permission check every one of
// the 22 Code RPCs requires, including the read-only ones — there is no
// narrower per-RPC permission and no read-only exemption. Unlike Documents
// and Settings, which are always available, Code is a gated facet — the
// same posture Inventory ships.
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

// The remaining nineteen forwarders resolve through codeServer's embedded
// hostv1.UnimplementedCodeServer until later plans in this phase implement
// their bodies (codes.Unimplemented in the meantime) — the gate below runs
// first regardless, so the denial behavior is complete now even though the
// inner behavior is not.

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
