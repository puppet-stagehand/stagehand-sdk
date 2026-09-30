package local

// This file holds the nine Hiera RPC bodies (GetHieraHierarchy,
// PutHieraLevel, RemoveHieraLevel, ReorderHieraLevels,
// ListHieraDataFiles, GetHieraDataFile, PutHieraDataKey,
// RemoveHieraDataKey, DeleteHieraDataFile) on *codeServer, split out of
// code.go: codeServer, gatedCode and all 27 forwarders live in code.go so
// the permission gate stays reviewable as a unit, while the RPC bodies are
// split by resource kind so the plans that write them (this one, and the
// sibling Puppetfile plan) can land in the same wave without touching the
// same file. Task 1 (below) builds the hierarchy RPCs plus
// dataFileTextLocked, the one data-file helper the hierarchy side needs
// for HIERA-03's read-only lookup_options mirroring; Task 2 adds the
// remaining data-file write helper and the five data-file RPC bodies.

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// --- hierarchy (hiera.yaml) helpers ---

// hierarchyTextLocked reads the code-hiera-hierarchy document's "yaml"
// field for env, reporting whether a document existed. An absent document
// is not an error: an environment whose hiera.yaml has not been authored
// yet is a legitimate state the read path must be able to report. The
// caller must already hold s.docs.mu.
func (s *codeServer) hierarchyTextLocked(env string) (string, bool) {
	doc, ok := s.docs.getLocked(hieraHierarchyCollection, env)
	if !ok || doc.Body == nil || doc.Body.Value == nil {
		return "", ok
	}
	text, _ := doc.Body.Value.AsMap()["yaml"].(string)
	return text, ok
}

// storeHierarchyLocked writes yamlText into the code-hiera-hierarchy
// document's "yaml" key for env. The caller must already hold s.docs.mu.
func (s *codeServer) storeHierarchyLocked(env, yamlText string) error {
	st, err := structpb.NewStruct(map[string]any{"yaml": yamlText})
	if err != nil {
		return status.Errorf(codes.Internal, "building hiera hierarchy document body: %v", err)
	}
	if _, err := s.docs.putLocked(hieraHierarchyCollection, env, &hostv1.Json{Value: st}, false); err != nil {
		return err
	}
	return nil
}

// mapHieraErr maps the code package's Hiera sentinel errors onto gRPC
// codes via errors.Is — never by matching the text of the error message,
// which breaks silently the first time a message is reworded.
func mapHieraErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, code.ErrHieraInvalid):
		return status.Errorf(codes.InvalidArgument, "%v", err)
	case errors.Is(err, code.ErrHieraParse):
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}

// dataFileTextLocked reads the code-hiera-data document's "yaml" field for
// env/path, reporting whether a document existed. It builds its storage
// key through hieraDataKey — never by concatenating strings at the call
// site. mirrorLookupOptionsLocked (below) is this helper's first caller;
// Task 2 of this plan adds the five data-file RPC bodies that are its
// remaining callers. The caller must already hold s.docs.mu.
func (s *codeServer) dataFileTextLocked(env, path string) (string, bool) {
	doc, ok := s.docs.getLocked(hieraDataCollection, hieraDataKey(env, path))
	if !ok || doc.Body == nil || doc.Body.Value == nil {
		return "", ok
	}
	text, _ := doc.Body.Value.AsMap()["yaml"].(string)
	return text, ok
}

// mirrorLookupOptionsLocked implements HIERA-03's display-only
// lookup_options surfacing on a hierarchy's levels. For each level, if and
// only if its path is non-empty and contains no interpolation token
// (%{...}), the data file it literally names is read (when present) and
// parsed, and the resulting lookup_options map is copied onto the level.
// Every other level — an interpolated path, or a level using
// paths/glob/mapped_paths instead of path — gets an empty map. This
// function never substitutes a fact, never globs, and never evaluates a
// merge: HIERA-03 is read-for-display only this milestone, and a real
// Hiera lookup might resolve a level's path differently once facts are
// involved. The caller must already hold s.docs.mu.
func (s *codeServer) mirrorLookupOptionsLocked(env string, h *hostv1.HieraHierarchy) {
	for _, lvl := range h.GetLevels() {
		path := lvl.GetPath()
		if path == "" || strings.Contains(path, "%{") {
			lvl.LookupOptions = map[string]string{}
			continue
		}
		text, ok := s.dataFileTextLocked(env, path)
		if !ok {
			lvl.LookupOptions = map[string]string{}
			continue
		}
		df, err := code.ParseDataFile(text)
		if err != nil {
			lvl.LookupOptions = map[string]string{}
			continue
		}
		lvl.LookupOptions = df.LookupOptions
	}
}

// GetHieraHierarchy reads and parses the stored hiera.yaml text (the empty
// string when unauthored yields a zero-level hierarchy), sets Environment,
// mirrors lookup_options for display, and returns a clone.
func (s *codeServer) GetHieraHierarchy(ctx context.Context, req *hostv1.GetHieraHierarchyRequest) (*hostv1.HieraHierarchy, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	text, _ := s.hierarchyTextLocked(req.Environment)
	h, err := code.ParseHierarchy(text)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	h.Environment = req.Environment
	s.mirrorLookupOptionsLocked(req.Environment, h)
	return proto.Clone(h).(*hostv1.HieraHierarchy), nil
}

// PutHieraLevel rejects a nil req.Level or an empty req.Level.Name, then
// rejects a level carrying a non-empty LookupOptions — that is HIERA-03's
// write-side half, and it comes before anything else so a refused write
// cannot have touched the document. It then runs code.LintLevelPaths and
// keeps the result before ever touching storage. The order matters and is
// the whole of D-01: the warnings are computed, the write is performed,
// and then both travel back together on the same response. A warning
// never becomes a refusal, never downgrades the write to a partial one,
// and never causes the level to be silently rewritten into a "safe" form.
//
// A level whose name is already in the hierarchy is an overwrite and is
// refused unless covered by an approved proposal, and even then only
// ApplyHieraLevelOverwrite materializes it. A level with a new name is
// ungated create. The gate follows the environment NotFound check and the
// read-only lookup_options refusal, so those keep their own reasons.
func (s *codeServer) PutHieraLevel(ctx context.Context, req *hostv1.PutHieraLevelRequest) (*hostv1.PutHieraLevelResponse, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}
	if req.Level == nil || req.Level.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "level and level name are required")
	}
	if len(req.Level.GetLookupOptions()) > 0 {
		return nil, status.Error(codes.InvalidArgument, "lookup_options is read-only and cannot be set by a write")
	}

	warnings := code.LintLevelPaths(req.Level)

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	text, _ := s.hierarchyTextLocked(req.Environment)

	// Overwrite gate (D-02): the existence check runs against the hierarchy's
	// node tree (names only, no other level converted) before code.PutLevel is
	// ever called, so a refused write never reaches it and an unrelated level
	// that cannot be parsed cannot block adding a new one. Adding a level whose name is not present stays
	// ungated create.
	exists, err := code.LevelExists(text, req.Level.GetName())
	if err != nil {
		return nil, mapHieraErr(err)
	}
	if exists {
		target := code.OverwriteTarget{
			Environment: req.Environment,
			Resource:    code.OverwriteResourceHieraLevel,
			Name:        req.Level.GetName(),
		}
		if id, approved := s.approvedOverwriteProposalLocked(target); approved {
			return nil, ErrCodeOverwriteApplyPending(id, applyHieraLevelRPC)
		}
		return nil, ErrCodeOverwriteRequiresApproval(target, applyHieraLevelRPC)
	}

	newText, err := code.PutLevel(text, req.Level, req.Index, req.Insert)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	if err := s.storeHierarchyLocked(req.Environment, newText); err != nil {
		return nil, err
	}

	stored, _ := s.hierarchyTextLocked(req.Environment)
	h, err := code.ParseHierarchy(stored)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	h.Environment = req.Environment
	s.mirrorLookupOptionsLocked(req.Environment, h)

	return &hostv1.PutHieraLevelResponse{
		Hierarchy: proto.Clone(h).(*hostv1.HieraHierarchy),
		Warnings:  warnings,
	}, nil
}

// RemoveHieraLevel drops the named level, delegating name-matching
// entirely to code.RemoveLevel — which is what keeps "common" from
// matching "common_extra": the format package compares a level's name
// node for exact equality, and no prefix comparison exists anywhere on
// this path.
func (s *codeServer) RemoveHieraLevel(ctx context.Context, req *hostv1.RemoveHieraLevelRequest) (*hostv1.HieraHierarchy, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	text, _ := s.hierarchyTextLocked(req.Environment)
	newText, err := code.RemoveLevel(text, req.Name)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	if err := s.storeHierarchyLocked(req.Environment, newText); err != nil {
		return nil, err
	}

	stored, _ := s.hierarchyTextLocked(req.Environment)
	h, err := code.ParseHierarchy(stored)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	h.Environment = req.Environment
	s.mirrorLookupOptionsLocked(req.Environment, h)
	return proto.Clone(h).(*hostv1.HieraHierarchy), nil
}

// ReorderHieraLevels applies req.Names as the new level order, delegating
// permutation validation entirely to code.ReorderLevels: a names list
// that is not an exact permutation returns codes.InvalidArgument and
// leaves the stored document byte-identical, since the format package
// never touches the sequence until the permutation is proven valid.
func (s *codeServer) ReorderHieraLevels(ctx context.Context, req *hostv1.ReorderHieraLevelsRequest) (*hostv1.HieraHierarchy, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	text, _ := s.hierarchyTextLocked(req.Environment)
	newText, err := code.ReorderLevels(text, req.Names)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	if err := s.storeHierarchyLocked(req.Environment, newText); err != nil {
		return nil, err
	}

	stored, _ := s.hierarchyTextLocked(req.Environment)
	h, err := code.ParseHierarchy(stored)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	h.Environment = req.Environment
	s.mirrorLookupOptionsLocked(req.Environment, h)
	return proto.Clone(h).(*hostv1.HieraHierarchy), nil
}

// --- data files (data/*.yaml) helpers and RPCs (Task 2) ---

// storeDataFileLocked writes yamlText into the code-hiera-data document
// for env/path, alongside a "path" field so a listing can report the
// relative path without re-splitting every doc id. Reading a listing still
// goes through hieraDataEnvAndPath on the doc id, because that is the
// authoritative ownership rule; the stored "path" field is a convenience,
// not a second source of truth, and the two must agree. The caller must
// already hold s.docs.mu.
func (s *codeServer) storeDataFileLocked(env, path, yamlText string) error {
	st, err := structpb.NewStruct(map[string]any{"path": path, "yaml": yamlText})
	if err != nil {
		return status.Errorf(codes.Internal, "building hiera data document body: %v", err)
	}
	if _, err := s.docs.putLocked(hieraDataCollection, hieraDataKey(env, path), &hostv1.Json{Value: st}, false); err != nil {
		return err
	}
	return nil
}

// ListHieraDataFiles walks the code-hiera-data collection, keeps only ids
// whose hieraDataEnvAndPath split yields this environment — deciding
// ownership by the separator-anchored split rather than a bare name
// prefix, which is what keeps "prod"'s listing free of "production"'s
// files — collects the relative paths (already ascending, since
// idsLocked's sorted ids share a fixed environment prefix once filtered),
// and paginates with the shared pageBounds helper.
func (s *codeServer) ListHieraDataFiles(ctx context.Context, req *hostv1.ListHieraDataFilesRequest) (*hostv1.ListHieraDataFilesResponse, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	var allPaths []string
	for _, id := range s.docs.idsLocked(hieraDataCollection) {
		if env, path, ok := hieraDataEnvAndPath(id); ok && env == req.Environment {
			allPaths = append(allPaths, path)
		}
	}

	start, limit, err := pageBounds(req.Page, len(allPaths))
	if err != nil {
		return nil, err
	}

	paths := make([]string, 0, limit)
	end := start
	for end < len(allPaths) && len(paths) < limit {
		paths = append(paths, allPaths[end])
		end++
	}
	pageInfo := &hostv1.PageInfo{}
	if end < len(allPaths) {
		pageInfo.NextCursor = strconv.Itoa(end)
	}
	return &hostv1.ListHieraDataFilesResponse{Paths: paths, Page: pageInfo}, nil
}

// GetHieraDataFile reads the stored text, returns codes.NotFound when
// absent, parses it, sets Environment and Path, and returns it cloned.
func (s *codeServer) GetHieraDataFile(ctx context.Context, req *hostv1.GetHieraDataFileRequest) (*hostv1.HieraDataFile, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}
	if err := code.ValidateDataPath(req.Path); err != nil {
		return nil, mapHieraErr(err)
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	text, ok := s.dataFileTextLocked(req.Environment, req.Path)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "environment %q has no hiera data file %q", req.Environment, req.Path)
	}
	df, err := code.ParseDataFile(text)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	df.Environment = req.Environment
	df.Path = req.Path
	return proto.Clone(df).(*hostv1.HieraDataFile), nil
}

// PutHieraDataKey rejects an empty req.Key, reads the stored text (the
// empty string when absent, which code.PutDataKey treats as a new
// document), calls code.PutDataKey — which is where the reserved-key
// refusal for "lookup_options" surfaces as codes.InvalidArgument — stores
// the returned text, and returns the re-read file. The whole edit is
// routed through the format package's node-tree function and exactly the
// text it returns is stored; nothing here parses the file into a Go map,
// mutates that, and re-serialises it, which would drop every comment and
// can reorder keys.
//
// A key already present in the file is an overwrite and is refused unless an
// approved proposal for that environment, file and key exists, and even then
// only ApplyHieraDataKeyOverwrite materializes it. A new key is ungated
// create. The gate is checked before code.PutDataKey is called.
func (s *codeServer) PutHieraDataKey(ctx context.Context, req *hostv1.PutHieraDataKeyRequest) (*hostv1.HieraDataFile, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}
	if err := code.ValidateDataPath(req.Path); err != nil {
		return nil, mapHieraErr(err)
	}
	if req.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	text, present := s.dataFileTextLocked(req.Environment, req.Path)

	// Overwrite gate (D-02): only a key already present in the file is an
	// overwrite. An absent file, or a key not in it, stays ungated create. The
	// existence check reads the node tree's keys only and converts no value, so
	// a value ParseDataFile could not represent cannot block adding an unrelated
	// key. The reserved lookup_options key reports absent, so it falls through
	// to code.PutDataKey's own refusal.
	if present {
		exists, err := code.DataKeyExists(text, req.Key)
		if err != nil {
			return nil, mapHieraErr(err)
		}
		if exists {
			target := code.OverwriteTarget{
				Environment: req.Environment,
				Resource:    code.OverwriteResourceHieraDataKey,
				Name:        req.Key,
				Path:        req.Path,
			}
			if id, approved := s.approvedOverwriteProposalLocked(target); approved {
				return nil, ErrCodeOverwriteApplyPending(id, applyHieraDataKeyRPC)
			}
			return nil, ErrCodeOverwriteRequiresApproval(target, applyHieraDataKeyRPC)
		}
	}

	newText, err := code.PutDataKey(text, req.Key, req.Value)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	if err := s.storeDataFileLocked(req.Environment, req.Path, newText); err != nil {
		return nil, err
	}

	stored, _ := s.dataFileTextLocked(req.Environment, req.Path)
	df, err := code.ParseDataFile(stored)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	df.Environment = req.Environment
	df.Path = req.Path
	return proto.Clone(df).(*hostv1.HieraDataFile), nil
}

// RemoveHieraDataKey mirrors PutHieraDataKey over code.RemoveDataKey: an
// unknown path returns codes.NotFound (checked before the format package
// is ever invoked) and an unknown key returns codes.InvalidArgument (via
// mapHieraErr on code.RemoveDataKey's ErrHieraInvalid).
func (s *codeServer) RemoveHieraDataKey(ctx context.Context, req *hostv1.RemoveHieraDataKeyRequest) (*hostv1.HieraDataFile, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}
	if err := code.ValidateDataPath(req.Path); err != nil {
		return nil, mapHieraErr(err)
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	text, ok := s.dataFileTextLocked(req.Environment, req.Path)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "environment %q has no hiera data file %q", req.Environment, req.Path)
	}
	newText, err := code.RemoveDataKey(text, req.Key)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	if err := s.storeDataFileLocked(req.Environment, req.Path, newText); err != nil {
		return nil, err
	}

	stored, _ := s.dataFileTextLocked(req.Environment, req.Path)
	df, err := code.ParseDataFile(stored)
	if err != nil {
		return nil, mapHieraErr(err)
	}
	df.Environment = req.Environment
	df.Path = req.Path
	return proto.Clone(df).(*hostv1.HieraDataFile), nil
}

// DeleteHieraDataFile removes the whole document through the lock-free
// deleter, returning codes.NotFound when it was not present.
func (s *codeServer) DeleteHieraDataFile(ctx context.Context, req *hostv1.DeleteHieraDataFileRequest) (*emptypb.Empty, error) {
	if err := validateEnvName(req.Environment); err != nil {
		return nil, err
	}
	if err := code.ValidateDataPath(req.Path); err != nil {
		return nil, mapHieraErr(err)
	}

	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	if _, ok := s.docs.getLocked(envCollection, req.Environment); !ok {
		return nil, status.Errorf(codes.NotFound, "no environment %q", req.Environment)
	}

	if !s.docs.deleteLocked(hieraDataCollection, hieraDataKey(req.Environment, req.Path)) {
		return nil, status.Errorf(codes.NotFound, "environment %q has no hiera data file %q", req.Environment, req.Path)
	}
	return &emptypb.Empty{}, nil
}
