package local

// forge_resolver.go implements the pure, advisory dependency-tree walk
// FORGE-04/05 require: a recursion-path cycle guard (D-09/D-10), a
// per-walk metadata/version cache (D-04), and a cross-path constraint
// ledger that flags incompatible SemVer requirements without changing
// which version each individual path resolved to (D-11, per 07-RESEARCH.md
// Pattern 2: "Do NOT use a global visited set alone: it would hide a
// second path and prevent conflict provenance").
//
// This file has no dependency on Documents/Secrets/Code or forgeServer: it
// only knows ForgeClient. forge.go's Resolve RPC (07-03 Task 2) supplies
// the endpoint/source and attaches already_in_puppetfile to the tree this
// file returns — nothing here reads or writes any facet state, matching
// D-14/FORGE-06 (Forge never touches a Puppetfile).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	semver "github.com/Masterminds/semver/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// forgeMaxResolveNodes bounds how many dependency edges one Resolve call may
// expand. Release metadata is cached but the tree is not memoised, so a
// diamond-heavy graph (each module depending on the next twice) would
// otherwise build 2^N nodes from O(N) HTTP calls (WR-03). Exceeding it aborts
// the walk with ResourceExhausted rather than exhausting host memory.
const forgeMaxResolveNodes = 5000

// forgeResolveWalk holds the state of exactly one Resolve call: the
// client/endpoint metadata is fetched through, a (name,version) release
// cache and a per-name version-list cache (D-04, scoped to this call only
// — never persisted across RPCs), and a constraint ledger recording every
// dependency edge's (requirement, node, origin) so detectConflicts can
// run once the whole tree is known. It is not safe for concurrent use: the
// walk is a single-threaded DFS.
type forgeResolveWalk struct {
	ctx    context.Context
	client ForgeClient
	ep     ForgeEndpoint
	source string

	releaseCache map[forgeCacheKey]releaseCacheEntry
	versionCache map[string]versionCacheEntry
	ledger       map[string][]ledgerEntry

	// fatal is set the first time getRelease/listReleases returns anything
	// other than ErrForgeNotFound — a real transport/malformed-payload
	// failure (D-03), which aborts the whole walk rather than becoming
	// advisory data. Only a typed not-found is advisory (D-12).
	fatal error

	// nodes counts the dependency edges expanded so far, against
	// forgeMaxResolveNodes.
	nodes int
}

type forgeCacheKey struct{ name, version string }

type releaseCacheEntry struct {
	release  *ForgeRelease
	notFound bool
}

type versionCacheEntry struct {
	versions []string
	notFound bool
}

// ledgerEntry is one dependency edge's outcome, recorded regardless of
// whether it resolved cleanly, so detectConflicts can find every path that
// named the same module by its ledger key.
type ledgerEntry struct {
	requirement string
	node        *hostv1.DependencyNode
	origin      string
}

func newForgeResolveWalk(ctx context.Context, client ForgeClient, ep ForgeEndpoint, source string) *forgeResolveWalk {
	return &forgeResolveWalk{
		ctx: ctx, client: client, ep: ep, source: source,
		releaseCache: map[forgeCacheKey]releaseCacheEntry{},
		versionCache: map[string]versionCacheEntry{},
		ledger:       map[string][]ledgerEntry{},
	}
}

// resolveDependencyTree walks root's full transitive dependency graph
// (D-09, no artificial depth cap — only the recursion-path cycle guard
// bounds it) and returns the reviewable advisory tree (D-13). A typed
// not-found anywhere in the tree, root included, becomes an unresolved
// node rather than a whole-RPC failure (D-12); any other transport or
// malformed-payload error aborts the walk and is returned as-is so the
// caller can map it to the correct gRPC status (D-03).
func resolveDependencyTree(ctx context.Context, client ForgeClient, ep ForgeEndpoint, source, rootName, rootVersion string) (*hostv1.DependencyNode, error) {
	w := newForgeResolveWalk(ctx, client, ep, source)
	root := w.walkKnownVersion(rootName, rootVersion, nil)
	if w.fatal != nil {
		return nil, w.fatal
	}
	w.detectConflicts()
	return root, nil
}

// getRelease fetches (name, version)'s metadata through the per-walk cache
// (D-04): a module/version repeated across branches costs exactly one
// client call. ErrForgeNotFound is cached and returned as-is; any other
// error is recorded as fatal and returned.
func (w *forgeResolveWalk) getRelease(name, version string) (*ForgeRelease, error) {
	if w.fatal != nil {
		return nil, w.fatal
	}
	key := forgeCacheKey{name: name, version: version}
	if entry, ok := w.releaseCache[key]; ok {
		if entry.notFound {
			return nil, ErrForgeNotFound
		}
		return entry.release, nil
	}
	rel, err := w.client.GetRelease(w.ctx, w.ep, name, version)
	if err != nil {
		if errors.Is(err, ErrForgeNotFound) {
			w.releaseCache[key] = releaseCacheEntry{notFound: true}
			return nil, ErrForgeNotFound
		}
		w.fatal = err
		return nil, err
	}
	w.releaseCache[key] = releaseCacheEntry{release: rel}
	return rel, nil
}

// listReleases fetches name's known release versions through the per-walk
// cache, with the same not-found/fatal split as getRelease.
func (w *forgeResolveWalk) listReleases(name string) ([]string, error) {
	if w.fatal != nil {
		return nil, w.fatal
	}
	if entry, ok := w.versionCache[name]; ok {
		if entry.notFound {
			return nil, ErrForgeNotFound
		}
		return entry.versions, nil
	}
	versions, err := w.client.ListReleases(w.ctx, w.ep, name)
	if err != nil {
		if errors.Is(err, ErrForgeNotFound) {
			w.versionCache[name] = versionCacheEntry{notFound: true}
			return nil, ErrForgeNotFound
		}
		w.fatal = err
		return nil, err
	}
	w.versionCache[name] = versionCacheEntry{versions: versions}
	return versions, nil
}

// walkKnownVersion builds the node for a module whose exact version is
// already decided — the root (caller-specified) or a dependency edge that
// already picked a version via resolveEdge — and recurses into its
// declared dependencies. path is the chain of module NAMES from the root
// down to (but not including) name: name already present in path is a
// cycle (D-10), checked before any client call so a cyclic edge never
// re-fetches metadata it already has.
func (w *forgeResolveWalk) walkKnownVersion(name, version string, path []string) *hostv1.DependencyNode {
	// A module identity may arrive as ns/name or ns-name; normalise so the
	// cycle guard, caches and ledger agree on one spelling (WR-02).
	name = normalizeModuleName(name)
	node := &hostv1.DependencyNode{Name: name, Version: version, Source: w.source}
	if containsString(path, name) {
		node.Cycle = true
		node.Warnings = append(node.Warnings, pathWarning("cycle", name, "", path, fmt.Sprintf("cyclic dependency on %s", name)))
		return node
	}

	rel, err := w.getRelease(name, version)
	if err != nil {
		if errors.Is(err, ErrForgeNotFound) {
			node.Unresolved = true
			node.Warnings = append(node.Warnings, pathWarning("unresolved", name, "", path, "module or release not found"))
		}
		// A non-not-found error means w.fatal is now set; the caller
		// (resolveDependencyTree, or resolveEdge's own fatal check) is
		// responsible for aborting — this node is discarded either way.
		return node
	}

	childPath := append(append([]string{}, path...), name)
	for _, dep := range rel.Dependencies {
		if w.fatal != nil {
			break
		}
		node.Dependencies = append(node.Dependencies, w.resolveEdge(dep.Name, dep.VersionRequirement, childPath))
	}
	return node
}

// resolveEdge handles one declared dependency (name, requirement): cycle
// check, requirement parsing (D-11's "invalid requirements are retained as
// advisory warnings"), candidate selection via listReleases + SemVer
// filtering, and recursion into the chosen version. Every outcome is
// recorded into the ledger (even a cycle/invalid/unresolved one) so a
// caller reviewing the tree can always find every path that named a given
// module, and so detectConflicts has full data once the walk completes.
func (w *forgeResolveWalk) resolveEdge(name, requirement string, path []string) *hostv1.DependencyNode {
	// A cancelled call must stop walking cached subtrees, and the tree size
	// is bounded (WR-03). Either sets fatal so every enclosing loop unwinds.
	if w.fatal == nil {
		if err := w.ctx.Err(); err != nil {
			w.fatal = status.FromContextError(err).Err()
		} else if w.nodes++; w.nodes > forgeMaxResolveNodes {
			w.fatal = status.Errorf(codes.ResourceExhausted, "forge: dependency tree exceeds the adapter's limit of %d edges", forgeMaxResolveNodes)
		}
	}
	if w.fatal != nil {
		return &hostv1.DependencyNode{Name: normalizeModuleName(name), Source: w.source, VersionRequirement: requirement}
	}
	// Normalise the spelling (ns/name vs ns-name) before it keys the cycle
	// guard, caches and ledger (WR-02).
	name = normalizeModuleName(name)
	origin := strings.Join(append(append([]string{}, path...), name), " > ")

	if containsString(path, name) {
		node := &hostv1.DependencyNode{Name: name, Source: w.source, VersionRequirement: requirement, Cycle: true}
		node.Warnings = append(node.Warnings, pathWarning("cycle", name, requirement, path, fmt.Sprintf("cyclic dependency on %s", name)))
		w.record(name, requirement, node, origin)
		return node
	}

	var constraint *semver.Constraints
	if requirement != "" {
		c, err := semver.NewConstraint(requirement)
		if err != nil {
			node := &hostv1.DependencyNode{Name: name, Source: w.source, VersionRequirement: requirement, Unresolved: true}
			node.Warnings = append(node.Warnings, &hostv1.ForgeAdvisoryWarning{
				Code: "invalid_range", Module: name, Requirement: requirement, Origins: []string{origin},
				Message: fmt.Sprintf("invalid version requirement %q: %v", requirement, err),
			})
			w.record(name, requirement, node, origin)
			return node
		}
		constraint = c
	}

	if w.fatal != nil {
		return &hostv1.DependencyNode{Name: name, Source: w.source, VersionRequirement: requirement}
	}

	versions, err := w.listReleases(name)
	if err != nil {
		if !errors.Is(err, ErrForgeNotFound) {
			return &hostv1.DependencyNode{Name: name, Source: w.source, VersionRequirement: requirement}
		}
		node := &hostv1.DependencyNode{Name: name, Source: w.source, VersionRequirement: requirement, Unresolved: true}
		node.Warnings = append(node.Warnings, pathWarning("unresolved", name, requirement, path, "module not found"))
		w.record(name, requirement, node, origin)
		return node
	}

	chosen, ok := highestSatisfying(versions, constraint)
	if !ok {
		node := &hostv1.DependencyNode{Name: name, Source: w.source, VersionRequirement: requirement, Unresolved: true}
		node.Warnings = append(node.Warnings, pathWarning("unresolved", name, requirement, path,
			fmt.Sprintf("no known release of %s satisfies %q", name, requirement)))
		w.record(name, requirement, node, origin)
		return node
	}

	node := w.walkKnownVersion(name, chosen, path)
	node.VersionRequirement = requirement
	w.record(name, requirement, node, origin)
	return node
}

// record appends one edge's outcome to the constraint ledger, keyed by
// module name.
func (w *forgeResolveWalk) record(name, requirement string, node *hostv1.DependencyNode, origin string) {
	w.ledger[name] = append(w.ledger[name], ledgerEntry{requirement: requirement, node: node, origin: origin})
}

// detectConflicts runs once, after the full tree has been walked (D-11).
// For every module name reached via two or more DISTINCT requirement
// strings, it tests whether any cached candidate version satisfies every
// valid accumulated constraint simultaneously. If none does, every entry
// sharing that module name gets a conflict warning naming all the origins
// involved. This never changes which version any individual edge already
// resolved to — each path keeps the best version for ITS OWN requirement
// (07-RESEARCH.md: "retains all dependency paths" rather than silently
// picking one version that satisfies only some of them).
func (w *forgeResolveWalk) detectConflicts() {
	for name, entries := range w.ledger {
		if len(entries) < 2 {
			continue
		}
		distinct := map[string]bool{}
		for _, e := range entries {
			distinct[e.requirement] = true
		}
		if len(distinct) <= 1 {
			continue // every path wants the same thing — not a conflict
		}

		var constraints []*semver.Constraints
		for req := range distinct {
			if req == "" {
				continue // unconstrained never narrows an intersection
			}
			c, err := semver.NewConstraint(req)
			if err != nil {
				continue // already flagged invalid_range on its own node
			}
			constraints = append(constraints, c)
		}
		if len(constraints) < 2 {
			continue // fewer than 2 valid, distinct constraints can't conflict
		}

		cached, ok := w.versionCache[name]
		if !ok || cached.notFound {
			continue // no known candidate set to test an intersection against
		}

		satisfiable := false
		for _, raw := range cached.versions {
			v, err := semver.NewVersion(raw)
			if err != nil {
				continue
			}
			allSatisfy := true
			for _, c := range constraints {
				if !c.Check(v) {
					allSatisfy = false
					break
				}
			}
			if allSatisfy {
				satisfiable = true
				break
			}
		}
		if satisfiable {
			continue
		}

		origins := make([]string, 0, len(entries))
		for _, e := range entries {
			origins = append(origins, e.origin)
		}
		sort.Strings(origins)
		for _, e := range entries {
			e.node.Conflict = true
			e.node.Warnings = append(e.node.Warnings, &hostv1.ForgeAdvisoryWarning{
				Code: "conflict", Module: name, Origins: origins,
				Message: fmt.Sprintf("incompatible version requirements for %s across %d paths", name, len(entries)),
			})
		}
	}
}

// highestSatisfying returns the highest SemVer version in versions
// satisfying constraint (nil constraint = unconstrained: every parseable
// version satisfies), tie-breaking equal-precedence versions by lexical
// string order for determinism (07-RESEARCH.md's SemVer candidate
// selection resolution). A version string that does not itself parse as
// SemVer is skipped — Forge is expected to publish valid SemVer, but one
// malformed release must not abort selection over every other candidate.
func highestSatisfying(versions []string, constraint *semver.Constraints) (string, bool) {
	var best *semver.Version
	var bestRaw string
	for _, raw := range versions {
		v, err := semver.NewVersion(raw)
		if err != nil {
			continue
		}
		if constraint != nil && !constraint.Check(v) {
			continue
		}
		if best == nil {
			best, bestRaw = v, raw
			continue
		}
		if cmp := v.Compare(best); cmp > 0 || (cmp == 0 && raw > bestRaw) {
			best, bestRaw = v, raw
		}
	}
	if best == nil {
		return "", false
	}
	return bestRaw, true
}

// pathWarning builds a *hostv1.ForgeAdvisoryWarning whose single Origins
// entry is path+name joined into one readable " > "-delimited string.
func pathWarning(code, name, requirement string, path []string, message string) *hostv1.ForgeAdvisoryWarning {
	origin := strings.Join(append(append([]string{}, path...), name), " > ")
	return &hostv1.ForgeAdvisoryWarning{
		Code: code, Module: name, Requirement: requirement, Message: message, Origins: []string{origin},
	}
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
