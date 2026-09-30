package local

// This file holds the five Puppetfile RPC bodies (ListPuppetfileModules,
// PutPuppetfileModule, RemovePuppetfileModule, SetModuledir,
// RenderPuppetfile) on *codeServer, split out of code.go: codeServer,
// gatedCode and all 22 forwarders live in code.go so the permission gate
// stays reviewable as a unit, while the RPC bodies are split by resource
// kind so the three plans that write them (this one, and the sibling Hiera
// plan) can land in the same wave without touching the same file.

import (
	"context"
	"errors"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// puppetfileTextLocked reads the code-puppetfiles document's "text" field
// for env, reporting whether a document existed. An absent document is not
// an error: an environment whose Puppetfile has not been authored yet is a
// legitimate state that the read path must be able to report, rather than
// forcing every caller to distinguish "no Puppetfile" from "no environment"
// by string-matching an error message. The caller must already hold
// s.docs.mu.
func (s *codeServer) puppetfileTextLocked(env string) (string, bool) {
	doc, ok := s.docs.getLocked(puppetfileCollection, env)
	if !ok || doc.Body == nil || doc.Body.Value == nil {
		return "", ok
	}
	text, _ := doc.Body.Value.AsMap()["text"].(string)
	return text, ok
}

// loadPuppetfileLocked is the read-and-parse half of every Puppetfile RPC
// body: it reads env's stored text and hands it to code.ParsePuppetfile. An
// absent document parses the empty string, which yields an empty model —
// no error. The caller must already hold s.docs.mu.
func (s *codeServer) loadPuppetfileLocked(env string) (*hostv1.Puppetfile, error) {
	text, _ := s.puppetfileTextLocked(env)
	pf, err := code.ParsePuppetfile(text)
	if err != nil {
		return nil, mapPuppetfileErr(err)
	}
	pf.Environment = env
	return pf, nil
}

// storePuppetfileLocked is the validate-render-write half of every write
// path: it renders p through code.RenderPuppetfile and writes the resulting
// text into the code-puppetfiles body's "text" key. The rendered text is
// what gets stored — the facet keeps no second, structured copy of the
// module list. One artifact means the stored document, the rendered view
// and the parsed model can never disagree with each other, and it is what
// makes PF-05's round-trip a property of every write instead of only of the
// format package's own tests. The caller must already hold s.docs.mu.
func (s *codeServer) storePuppetfileLocked(env string, p *hostv1.Puppetfile) error {
	text, err := code.RenderPuppetfile(p)
	if err != nil {
		return mapPuppetfileErr(err)
	}
	st, err := structpb.NewStruct(map[string]any{"text": text})
	if err != nil {
		return status.Errorf(codes.Internal, "building puppetfile document body: %v", err)
	}
	if _, err := s.docs.putLocked(puppetfileCollection, env, &hostv1.Json{Value: st}, false); err != nil {
		return err
	}
	return nil
}

// mapPuppetfileErr maps the code package's Puppetfile sentinel errors onto
// gRPC codes via errors.Is — never by matching the text of the error
// message, which breaks silently the first time a message is reworded.
func mapPuppetfileErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, code.ErrPuppetfileInvalid):
		return status.Errorf(codes.InvalidArgument, "%v", err)
	case errors.Is(err, code.ErrPuppetfileParse):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}

// clonePuppetfileModule returns a deep copy so a caller mutating the
// returned module can never reach into the facet's internal state. Every
// read path returns through this, never a pointer into stored state.
func clonePuppetfileModule(m *hostv1.PuppetfileModule) *hostv1.PuppetfileModule {
	return proto.Clone(m).(*hostv1.PuppetfileModule)
}

// ListPuppetfileModules loads the model, paginates Puppetfile.modules with
// the shared pageBounds helper, clones each returned module, and sets
// Moduledir on the response so a caller gets the whole picture in one call
// rather than needing a second round trip.
func (s *codeServer) ListPuppetfileModules(ctx context.Context, req *hostv1.ListPuppetfileModulesRequest) (*hostv1.ListPuppetfileModulesResponse, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	pf, err := s.loadPuppetfileLocked(req.Environment)
	if err != nil {
		return nil, err
	}

	start, limit, err := pageBounds(req.Page, len(pf.Modules))
	if err != nil {
		return nil, err
	}

	mods := make([]*hostv1.PuppetfileModule, 0, limit)
	end := start
	for end < len(pf.Modules) && len(mods) < limit {
		mods = append(mods, clonePuppetfileModule(pf.Modules[end]))
		end++
	}
	pageInfo := &hostv1.PageInfo{}
	if end < len(pf.Modules) {
		pageInfo.NextCursor = strconv.Itoa(end)
	}
	return &hostv1.ListPuppetfileModulesResponse{Modules: mods, Page: pageInfo, Moduledir: pf.Moduledir}, nil
}

// PutPuppetfileModule rejects a nil req.Module or an empty req.Module.Name
// with codes.InvalidArgument, then runs code.ValidateModule and maps its
// error — this is where D-02's and D-03's rules and the git-URL safety
// checks are enforced on a caller-supplied module. It then loads the model.
// A name not present is appended, ungated. A name already present is an
// overwrite and is refused with codes.FailedPrecondition (D-04): either
// ErrCodeOverwriteRequiresApproval, or ErrCodeOverwriteApplyPending when an
// approved code-overwrites proposal already covers it. A replacement
// happens only through ApplyPuppetfileModuleOverwrite, which keeps the
// module's index in the render order — a Puppetfile's order is the
// author's, and silently moving an edited module to the end would be a
// change nobody requested showing up as a diff.
func (s *codeServer) PutPuppetfileModule(ctx context.Context, req *hostv1.PutPuppetfileModuleRequest) (*hostv1.PuppetfileModule, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	if req.Module == nil || req.Module.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "module and module name are required")
	}
	if err := code.ValidateModule(req.Module); err != nil {
		return nil, mapPuppetfileErr(err)
	}

	pf, err := s.loadPuppetfileLocked(req.Environment)
	if err != nil {
		return nil, err
	}

	upserted := proto.Clone(req.Module).(*hostv1.PuppetfileModule)
	for _, m := range pf.Modules {
		if m.GetName() == upserted.GetName() {
			// Replacing an existing module is an overwrite, and an
			// overwrite is never applied silently (D-04). It is decided per
			// module, not per environment (D-02): the append below stays
			// ungated. The refusal happens before storePuppetfileLocked, so
			// a refused call leaves the stored text byte-identical. Even
			// when an approved proposal covers this module the Put still
			// refuses; materializing it is ApplyPuppetfileModuleOverwrite's
			// job, never this RPC's.
			target := code.OverwriteTarget{
				Environment: req.Environment,
				Resource:    code.OverwriteResourcePuppetfileModule,
				Name:        upserted.GetName(),
			}
			if proposalID, covered := s.approvedOverwriteProposalLocked(target); covered {
				return nil, ErrCodeOverwriteApplyPending(proposalID, applyPuppetfileModuleRPC)
			}
			return nil, ErrCodeOverwriteRequiresApproval(target, applyPuppetfileModuleRPC)
		}
	}
	pf.Modules = append(pf.Modules, upserted)

	if err := s.storePuppetfileLocked(req.Environment, pf); err != nil {
		return nil, err
	}
	return clonePuppetfileModule(upserted), nil
}

// RemovePuppetfileModule loads the model, finds the module by exact name,
// returns codes.NotFound when absent, removes it while preserving the order
// of the rest, stores, and returns an empty response.
func (s *codeServer) RemovePuppetfileModule(ctx context.Context, req *hostv1.RemovePuppetfileModuleRequest) (*emptypb.Empty, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	pf, err := s.loadPuppetfileLocked(req.Environment)
	if err != nil {
		return nil, err
	}

	idx := -1
	for i, m := range pf.Modules {
		if m.GetName() == req.Name {
			idx = i
			break
		}
	}
	if idx == -1 {
		return nil, status.Errorf(codes.NotFound, "environment %q has no puppetfile module %q", req.Environment, req.Name)
	}
	pf.Modules = append(pf.Modules[:idx], pf.Modules[idx+1:]...)

	if err := s.storePuppetfileLocked(req.Environment, pf); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// SetModuledir sets Puppetfile.moduledir and stores it. The empty string is
// a legitimate value meaning "no moduledir line" — it is not treated as a
// missing argument and is not rejected, because clearing the setting is how
// a pack undoes it and there is no separate clear RPC in the contract.
func (s *codeServer) SetModuledir(ctx context.Context, req *hostv1.SetModuledirRequest) (*hostv1.Puppetfile, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	pf, err := s.loadPuppetfileLocked(req.Environment)
	if err != nil {
		return nil, err
	}
	pf.Moduledir = req.Moduledir

	if err := s.storePuppetfileLocked(req.Environment, pf); err != nil {
		return nil, err
	}

	reloaded, err := s.loadPuppetfileLocked(req.Environment)
	if err != nil {
		return nil, err
	}
	return proto.Clone(reloaded).(*hostv1.Puppetfile), nil
}

// RenderPuppetfile returns the stored text verbatim through
// puppetfileTextLocked — the empty string when no document exists — rather
// than re-rendering the parsed model: the stored text already is the
// canonical render, because every write path in this file goes through
// storePuppetfileLocked. Re-rendering here would hide a divergence between
// the two instead of surfacing it.
func (s *codeServer) RenderPuppetfile(ctx context.Context, req *hostv1.RenderPuppetfileRequest) (*hostv1.RenderedPuppetfile, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	text, _ := s.puppetfileTextLocked(req.Environment)
	return &hostv1.RenderedPuppetfile{Text: text}, nil
}
