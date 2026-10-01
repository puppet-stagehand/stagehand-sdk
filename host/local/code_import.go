package local

// This file holds the Code facet's import flow: InspectImport reports a remote
// control repo and writes nothing, ProposeImport freezes the reviewed snapshot
// into one approval proposal, and ApplyImport materializes every selected
// branch atomically from that frozen content.
//
// The same structural rule as code_approval.go applies, and it is stricter
// here because Propose is the first place this package files a proposal.
// Nothing in this file decides a proposal. It composes the approval Kind from
// the code package's two constants, files a pending proposal, and later reads
// the status somebody else recorded. It never moves that status, never
// verifies a token and never evaluates an approval scope; TestImport_CannotDecide
// asserts that over the parsed source.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// importInspectTimeout bounds one whole inspect pipeline: credential reveal,
// branch discovery, fetch and parse. The git client has its own per-call
// bounds; this is the ceiling over all of them together.
const importInspectTimeout = 3 * time.Minute

// Detail codes attached to the two import refusals, read back by
// IsCodeImportBranchMoved and IsCodeImportUnflaggedCollision.
const (
	detailCodeImportBranchMoved        = "import_branch_moved"
	detailCodeImportUnflaggedCollision = "import_unflagged_collision"
)

// importRefusal builds a FailedPrecondition carrying a structured ErrorDetail.
// The detail is best-effort: the status itself must never fail to construct.
func importRefusal(detailCode, msg, fix string) error {
	st := status.New(codes.FailedPrecondition, msg)
	withDetails, err := st.WithDetails(&hostv1.ErrorDetail{Code: detailCode, Message: msg, Fix: fix})
	if err != nil {
		return st.Err()
	}
	return withDetails.Err()
}

// ErrCodeImportBranchMoved is the refusal ProposeImport returns when a selected
// branch's fetched SHA differs from the one the caller expected, so a proposal
// can never be filed against content different from the report the human read
// (D-06, DQ-3). It never echoes the repo URL.
func ErrCodeImportBranchMoved(branch, expected, actual string) error {
	return importRefusal(detailCodeImportBranchMoved,
		"branch "+quoteName(branch)+" is at commit "+actual+" but the report the caller read showed "+expected+"; it moved after that report",
		"run InspectImport again, have the new report reviewed, and propose against its commits")
}

// ErrCodeImportUnflaggedCollision is the refusal ApplyImport returns when a
// branch's environment now exists but the frozen snapshot did not flag the
// branch as an overwrite: the approver was never shown that environment being
// replaced (D-15). The whole import is refused. It never echoes the repo URL.
func ErrCodeImportUnflaggedCollision(branch string) error {
	return importRefusal(detailCodeImportUnflaggedCollision,
		"environment "+quoteName(branch)+" appeared after this import was proposed, so the approver never saw it being replaced; nothing was imported",
		"run InspectImport again so the report marks the overwrite, propose the import again and have it approved")
}

// IsCodeImportBranchMoved reports whether err is (or wraps) an
// ErrCodeImportBranchMoved error. It compares the ErrorDetail code and does
// not branch on codes.FailedPrecondition alone, which many refusals produce.
func IsCodeImportBranchMoved(err error) bool {
	return overwriteDetailCode(err) == detailCodeImportBranchMoved
}

// IsCodeImportUnflaggedCollision reports whether err is (or wraps) an
// ErrCodeImportUnflaggedCollision error.
func IsCodeImportUnflaggedCollision(err error) bool {
	return overwriteDetailCode(err) == detailCodeImportUnflaggedCollision
}

// ------------------------------------------------------------ adapter

// importRepoFS adapts one branch of a GitRepo onto the pure format layer's
// two-method code.ImportFS, so every decision about which files matter stays
// in package code and nothing git-shaped crosses into it. Entries keep their
// git mode, so a symlink or submodule pointer arrives as an entry for
// AnalyzeBranch to refuse rather than being filtered out here.
type importRepoFS struct {
	repo   GitRepo
	branch string
}

func (a importRepoFS) List(pathspecs ...string) ([]code.ImportFile, error) {
	entries, err := a.repo.ListFiles(a.branch, pathspecs...)
	if err != nil {
		return nil, err
	}
	out := make([]code.ImportFile, 0, len(entries))
	for _, e := range entries {
		out = append(out, code.ImportFile{Path: e.Path, Mode: e.Mode, Size: e.Size})
	}
	return out, nil
}

func (a importRepoFS) Read(path string) ([]byte, error) { return a.repo.ReadFile(a.branch, path) }

// ------------------------------------------------------------ credentials

// gitCredentialSecret is the sealed JSON shape a git credential Secret holds,
// an internal host.Local convention a pack must follow when it seals one, like
// llmProviderSecret. It is not part of the wire contract.
type gitCredentialSecret struct {
	Kind       string `json:"kind"`
	Username   string `json:"username"`
	Token      string `json:"token"`
	PrivateKey string `json:"private_key"`
}

// resolveGitCredential turns an explicit credential name into a GitCredential.
// There is no default and no index collection (D-02): the Secret ref is
// derived straight from the name with the secrets server's own composition,
// exactly one secret is revealed, and the plaintext lives only for the one
// fetch that follows. An empty name means an anonymous fetch and returns nil.
// An unknown name is NotFound; a payload that decodes but cannot be used is
// Internal, naming the credential and the broken field and never a value.
// docs.mu is not taken at all, so no lock can be held across the Reveal.
func (s *codeServer) resolveGitCredential(ctx context.Context, name string) (*GitCredential, error) {
	if name == "" {
		return nil, nil
	}
	revealed, err := s.secrets.Reveal(ctx, &hostv1.SecretRef{Ref: s.secrets.refFor(name)})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, status.Errorf(codes.NotFound, "git credential %q is not configured", name)
		}
		return nil, status.Errorf(codes.Internal, "git credential %q could not be revealed", name)
	}
	var sealed gitCredentialSecret
	if err := json.Unmarshal(revealed.Plaintext, &sealed); err != nil {
		return nil, status.Errorf(codes.Internal, "git credential %q is malformed: its secret is not a JSON object", name)
	}
	switch GitCredentialKind(sealed.Kind) {
	case GitCredentialHTTPSToken:
		if sealed.Token == "" {
			return nil, status.Errorf(codes.Internal, "git credential %q is malformed: field \"token\" is empty", name)
		}
	case GitCredentialSSHKey:
		if sealed.PrivateKey == "" {
			return nil, status.Errorf(codes.Internal, "git credential %q is malformed: field \"private_key\" is empty", name)
		}
	default:
		return nil, status.Errorf(codes.Internal, "git credential %q is malformed: field \"kind\" must be %q or %q", name, GitCredentialHTTPSToken, GitCredentialSSHKey)
	}
	return &GitCredential{
		Kind:       GitCredentialKind(sealed.Kind),
		Username:   sealed.Username,
		Token:      sealed.Token,
		PrivateKey: sealed.PrivateKey,
	}, nil
}

// ------------------------------------------------------------ pipeline

// importGitErr passes a git client's status error through unchanged, since the
// real client's ladder is static and never echoes the URL or a credential, and
// turns anything else into a static message rather than echoing its text.
func importGitErr(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "the git fetch timed out")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "the git fetch was canceled")
	}
	return status.Error(codes.Internal, "the git client failed")
}

// branchNameInvalidMessage words the D-14 finding. It states the rule that was
// violated and that real r10k would deploy the branch anyway under a corrected
// name, so an operator is not left wondering why a branch r10k accepts is
// refused here (RESEARCH Pitfall 15). This importer never renames, maps or
// sanitizes a branch.
func branchNameInvalidMessage(name string) string {
	return "branch name " + quoteName(name) + " does not match ^[a-z0-9_]+$ (lowercase letters, digits and underscore only), " +
		"so it cannot be an environment and is not imported. Real r10k's default would deploy this branch under a corrected name " +
		"(each character outside letters, digits and underscore replaced with an underscore); this importer never renames, so " +
		"push the branch under a valid name to import it"
}

func quoteName(s string) string {
	b, _ := json.Marshal(strings.ToValidUTF8(s, "�"))
	return string(b)
}

// snapshotBytes is the stored size of every text in a branch snapshot, the
// quantity the total snapshot ceiling bounds.
func snapshotBytes(b *hostv1.ImportBranchSnapshot) int64 {
	n := int64(len(b.GetPuppetfileText()) + len(b.GetHieraYaml()))
	for _, df := range b.GetDataFiles() {
		n += int64(len(df.GetYaml()))
	}
	return n
}

// analyzeImport is the whole no-lock pipeline shared by Inspect and Propose:
// resolve the credential, list the branches, classify each name against the
// environment-name rule before anything is fetched (D-14), fetch only the
// importable ones, analyze each through the pure layer, stamp the fetched SHA,
// and finally mark which branches would overwrite an existing environment.
// filter, when non-empty, narrows the discovered branches.
//
// docs.mu is held only for the final existence loop. It is never held across
// ListBranches, Open, AnalyzeBranch or Reveal: those can take minutes, and a
// lock held that long would stall every Documents call in the process
// (RESEARCH Pitfall 10).
func (s *codeServer) analyzeImport(ctx context.Context, url, credentialName string, filter []string) (*hostv1.ImportSnapshot, error) {
	if err := validateGitURL(url); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.inspectTimeout)
	defer cancel()

	cred, err := s.resolveGitCredential(ctx, credentialName)
	if err != nil {
		return nil, err
	}
	remote := GitRemote{URL: url, Credential: cred}

	refs, err := s.git.ListBranches(ctx, remote)
	if err != nil {
		return nil, importGitErr(err)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })

	// A name the caller asked for is never silently dropped (DQ-10): one the
	// remote does not have is refused before anything is fetched.
	discovered := make(map[string]bool, len(refs))
	for _, ref := range refs {
		discovered[ref.Name] = true
	}
	want := map[string]bool{}
	for _, n := range filter {
		if !discovered[n] {
			return nil, status.Errorf(codes.InvalidArgument, "branch %s was not found on the remote", quoteName(n))
		}
		want[n] = true
	}

	snap := &hostv1.ImportSnapshot{Url: url}
	var fetch []string
	for _, ref := range refs {
		if len(want) > 0 && !want[ref.Name] {
			continue
		}
		if err := validateEnvName(ref.Name); err != nil {
			// A name that violates the rule is reported and never fetched: it is
			// not added to the fetch list, so none of its bytes reach this host.
			snap.Branches = append(snap.Branches, &hostv1.ImportBranchSnapshot{
				Branch:     strings.ToValidUTF8(ref.Name, "�"),
				Importable: false,
				Findings: []*hostv1.ImportFinding{{
					Branch:   strings.ToValidUTF8(ref.Name, "�"),
					Kind:     code.FindingBranchNameInvalid,
					Severity: hostv1.ImportFinding_ERROR,
					Message:  branchNameInvalidMessage(ref.Name),
				}},
			})
			continue
		}
		fetch = append(fetch, ref.Name)
	}

	if len(fetch) > 0 {
		repo, err := s.git.Open(ctx, remote, fetch)
		if err != nil {
			return nil, importGitErr(err)
		}
		defer repo.Close()

		var total int64
		for _, name := range fetch {
			lim := s.importLimits
			// The data walk of each branch is given only what is left of the
			// total ceiling, so a late branch cannot be read past it.
			if remaining := s.importLimits.MaxSnapshotBytes - total; remaining > 0 {
				lim.MaxSnapshotBytes = remaining
			} else {
				lim.MaxSnapshotBytes = 1
			}
			b, _ := code.AnalyzeBranch(name, importRepoFS{repo: repo, branch: name}, lim)
			sha, ok := repo.Commit(name)
			if !ok {
				return nil, status.Error(codes.Internal, "the git client reported no commit for a fetched branch")
			}
			b.Commit = sha
			total += snapshotBytes(b)
			snap.Branches = append(snap.Branches, b)
		}
		if limit := s.importLimits.MaxSnapshotBytes; limit > 0 && total > limit {
			return nil, status.Errorf(codes.FailedPrecondition,
				"the selected branches hold %d bytes of importable content, over the %d byte snapshot ceiling; narrow the import with a branch list", total, limit)
		}
	}

	sort.Slice(snap.Branches, func(i, j int) bool { return snap.Branches[i].Branch < snap.Branches[j].Branch })

	func() {
		s.docs.mu.Lock()
		defer s.docs.mu.Unlock()
		for _, b := range snap.Branches {
			if b.Importable {
				b.WillOverwrite = s.targetInUseLocked(b.Branch)
			}
		}
	}()
	return snap, nil
}

// dedupeNames returns names without duplicates or empty strings, in first-seen
// order.
func dedupeNames(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// ------------------------------------------------------------ RPCs

// InspectImport clones the repo, parses every discovered branch and returns the
// per-branch report. It writes nothing: the only Documents access is the brief
// existence read that sets will_overwrite, and no proposal is created.
func (s *codeServer) InspectImport(ctx context.Context, req *hostv1.InspectImportRequest) (*hostv1.InspectImportResponse, error) {
	snap, err := s.analyzeImport(ctx, req.Url, req.Credential, dedupeNames(req.Branches))
	if err != nil {
		return nil, err
	}
	return &hostv1.InspectImportResponse{Snapshot: snap}, nil
}

// ProposeImport re-fetches the repo, freezes the parsed per-branch snapshot
// into one pending proposal in the shared code-overwrites collection, and
// returns the proposal id with the frozen snapshot. Nothing here can decide the
// proposal it files.
func (s *codeServer) ProposeImport(ctx context.Context, req *hostv1.ProposeImportRequest) (*hostv1.ProposeImportResponse, error) {
	if req.ProposalId == "" {
		return nil, status.Error(codes.InvalidArgument, "proposal_id is required")
	}
	filter := dedupeNames(req.Branches)
	snap, err := s.analyzeImport(ctx, req.Url, req.Credential, filter)
	if err != nil {
		return nil, err
	}

	frozen := &hostv1.ImportSnapshot{Url: snap.Url}
	importable := 0
	for _, b := range snap.Branches {
		switch {
		case b.Importable:
			importable++
			frozen.Branches = append(frozen.Branches, b)
		case len(filter) > 0:
			// The caller named this branch and it cannot be imported. Dropping it
			// would file a proposal the caller did not ask for (DQ-10).
			return nil, status.Errorf(codes.InvalidArgument, "branch %s was selected but is not importable: %s", quoteName(b.Branch), unimportableReason(b))
		case b.Branch != "" && !strings.Contains(b.Branch, ","):
			// The refused branches stay in the frozen snapshot, marked not
			// importable, so the approver reads the same findings the report
			// showed. ApplyImport skips them.
			frozen.Branches = append(frozen.Branches, b)
		}
	}
	if importable == 0 {
		return nil, status.Error(codes.FailedPrecondition, "no importable branch was selected, so there is nothing to propose")
	}

	// The optional staleness guard (DQ-3): when the caller pinned commits, every
	// selected branch must be pinned and must still be at that commit, so a
	// proposal can never be filed against content different from the report the
	// human read.
	if len(req.ExpectedCommits) > 0 {
		for _, b := range frozen.Branches {
			if !b.Importable {
				continue
			}
			want, ok := req.ExpectedCommits[b.Branch]
			if !ok {
				return nil, status.Errorf(codes.InvalidArgument,
					"expected_commits has no entry for selected branch %s; pin every selected branch or send no expected_commits", quoteName(b.Branch))
			}
			if want != b.Commit {
				return nil, ErrCodeImportBranchMoved(b.Branch, want, b.Commit)
			}
		}
	}

	body, err := code.OverwriteBodyForImport(frozen)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "import snapshot cannot be filed: %v", err)
	}
	// The Kind is composed from the code package's constants, never from the
	// request. ProposeBody reaches Documents.Put, which takes docs.mu, so it is
	// called here with the lock free.
	kind := approval.Kind{Collection: code.OverwriteCollection, ApproveScope: code.OverwriteApproveScope}
	if _, err := approval.ProposeBody(ctx, &host.Host{Documents: s.docs}, kind, req.ProposalId, body); err != nil {
		return nil, err
	}
	return &hostv1.ProposeImportResponse{ProposalId: req.ProposalId, Snapshot: proto.Clone(frozen).(*hostv1.ImportSnapshot)}, nil
}

// unimportableReason is the first finding's message for a refused branch, or a
// generic sentence when it somehow carries none.
func unimportableReason(b *hostv1.ImportBranchSnapshot) string {
	if len(b.GetFindings()) > 0 {
		return b.GetFindings()[0].GetMessage()
	}
	return "it carries no importable content"
}

// importEnvPlan is one environment ApplyImport will write: its name and every
// document it will own afterwards.
type importEnvPlan struct {
	name   string
	flag   bool // the snapshot's will_overwrite for this branch
	writes []plannedDocWrite
}

// buildImportPlan validates one branch's frozen content through the facet's own
// read paths and builds every document the environment will own, using exactly
// the body shapes those read paths expect. It mutates nothing and takes no
// lock, so ApplyImport can run it for every branch before the first write: that
// is what makes the write pass infallible.
func buildImportPlan(b *hostv1.ImportBranchSnapshot) (importEnvPlan, error) {
	name := b.GetBranch()
	plan := importEnvPlan{name: name, flag: b.GetWillOverwrite()}

	idBody, err := envDocBody(name)
	if err != nil {
		return plan, err
	}
	if b.GetSettings() != nil {
		settings := proto.Clone(b.GetSettings()).(*hostv1.EnvironmentSettings)
		settings.Environment = ""
		if err := checkSettingsRoundTrip(settings); err != nil {
			return plan, err
		}
		if idBody, err = settingsIntoBody(idBody, settings); err != nil {
			return plan, err
		}
	}
	plan.writes = append(plan.writes, plannedDocWrite{collection: envCollection, docID: name, body: idBody})

	if text := b.GetPuppetfileText(); text != "" {
		pf, err := code.ParsePuppetfile(text)
		if err != nil {
			return plan, mapPuppetfileErr(err)
		}
		for _, m := range pf.GetModules() {
			if err := code.ValidateModule(m); err != nil {
				return plan, mapPuppetfileErr(err)
			}
		}
		rendered, err := code.RenderPuppetfile(pf)
		if err != nil {
			return plan, mapPuppetfileErr(err)
		}
		back, err := code.ParsePuppetfile(rendered)
		if err != nil || !proto.Equal(back, pf) {
			return plan, status.Errorf(codes.FailedPrecondition, "branch %s: the frozen Puppetfile does not survive a render and strict re-parse unchanged", quoteName(name))
		}
		st, err := structpb.NewStruct(map[string]any{"text": rendered})
		if err != nil {
			return plan, status.Errorf(codes.Internal, "building puppetfile document body: %v", err)
		}
		plan.writes = append(plan.writes, plannedDocWrite{collection: puppetfileCollection, docID: name, body: &hostv1.Json{Value: st}})
	}

	if text := b.GetHieraYaml(); text != "" {
		if _, err := code.ParseHierarchy(text); err != nil {
			return plan, mapHieraErr(err)
		}
		st, err := structpb.NewStruct(map[string]any{"yaml": text})
		if err != nil {
			return plan, status.Errorf(codes.Internal, "building hierarchy document body: %v", err)
		}
		plan.writes = append(plan.writes, plannedDocWrite{collection: hieraHierarchyCollection, docID: name, body: &hostv1.Json{Value: st}})
	}

	seen := map[string]bool{}
	for _, df := range b.GetDataFiles() {
		if err := code.ValidateDataPath(df.GetPath()); err != nil {
			return plan, mapHieraErr(err)
		}
		if _, err := code.ParseDataFile(df.GetYaml()); err != nil {
			return plan, mapHieraErr(err)
		}
		if seen[df.GetPath()] {
			return plan, status.Errorf(codes.FailedPrecondition, "branch %s carries data file %q twice", quoteName(name), df.GetPath())
		}
		seen[df.GetPath()] = true
		st, err := structpb.NewStruct(map[string]any{"path": df.GetPath(), "yaml": df.GetYaml()})
		if err != nil {
			return plan, status.Errorf(codes.Internal, "building data file document body: %v", err)
		}
		plan.writes = append(plan.writes, plannedDocWrite{collection: hieraDataCollection, docID: hieraDataKey(name, df.GetPath()), body: &hostv1.Json{Value: st}})
	}
	return plan, nil
}

// ApplyImport materializes an approved import proposal: every importable branch
// in the frozen snapshot becomes an environment, or none does. It takes a
// proposal id and nothing else, makes no network call and needs no credential,
// so the content it writes is exactly the content the approver reviewed.
//
// The whole body runs under one s.docs.mu acquisition and uses only *Locked
// helpers. Pass one validates every branch and builds every write without
// mutating anything. If every environment already matches its plan the call is
// an idempotent no-op; if the proposal was already applied and anything has
// drifted it is refused, since an approval covers one application. Pass two
// contains only deletes and create-only puts after a delete in the same lock
// hold, so it cannot fail, and the applied marker is written last.
func (s *codeServer) ApplyImport(ctx context.Context, req *hostv1.ApplyImportRequest) (*hostv1.ApplyImportResponse, error) {
	s.docs.mu.Lock()
	defer s.docs.mu.Unlock()

	body, _, err := s.resolveApplyProposalLocked(req.ProposalId, code.OverwriteResourceImport)
	if err != nil {
		return nil, err
	}
	snap, err := code.OverwritePayloadImport(body)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q: %v", req.ProposalId, err)
	}

	// Pass one: validate everything, build every write, mutate nothing.
	var plans []importEnvPlan
	seen := map[string]bool{}
	for _, b := range snap.GetBranches() {
		if !b.GetImportable() {
			continue
		}
		name := b.GetBranch()
		if err := validateEnvName(name); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q names branch %s, which is not a valid environment name", req.ProposalId, quoteName(name))
		}
		if seen[name] {
			return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q names branch %s twice", req.ProposalId, quoteName(name))
		}
		seen[name] = true
		plan, err := buildImportPlan(b)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	if len(plans) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "overwrite proposal %q carries no importable branch", req.ProposalId)
	}

	allMatch := true
	for _, p := range plans {
		if !s.targetMatchesPlanLocked(s.envOwnedDocsLocked(p.name), p.writes) {
			allMatch = false
			break
		}
	}
	if allMatch {
		if err := s.markOverwriteAppliedLocked(req.ProposalId); err != nil {
			return nil, err
		}
		return s.importedEnvironmentsLocked(plans)
	}
	if s.overwriteAppliedLocked(req.ProposalId) {
		return nil, errOverwriteAlreadyApplied(req.ProposalId)
	}
	// The collision rule (D-15, RESEARCH Pitfall 8b). A branch the frozen
	// snapshot flagged as an overwrite is replaced; one it did not flag but
	// whose environment now exists was never shown to the approver as being
	// replaced, so the whole import is refused rather than that branch skipped.
	// The reverse, flagged but now absent, simply creates.
	for _, p := range plans {
		if !p.flag && s.targetInUseLocked(p.name) {
			return nil, ErrCodeImportUnflaggedCollision(p.name)
		}
	}

	// Pass two: only deletes and create-only puts. putLocked can fail only on a
	// create-only collision, and every id it writes was just deleted under this
	// same lock hold, so its error is unreachable and deliberately not returned:
	// a return here would abandon a half-written import.
	for _, p := range plans {
		for _, ref := range s.envOwnedDocsLocked(p.name) {
			s.docs.deleteLocked(ref.collection, ref.docID)
		}
		for _, w := range p.writes {
			_, _ = s.docs.putLocked(w.collection, w.docID, w.body, true)
		}
	}
	if err := s.markOverwriteAppliedLocked(req.ProposalId); err != nil {
		return nil, err
	}
	return s.importedEnvironmentsLocked(plans)
}

// importedEnvironmentsLocked reads back one Environment per planned branch. The
// caller must already hold s.docs.mu.
func (s *codeServer) importedEnvironmentsLocked(plans []importEnvPlan) (*hostv1.ApplyImportResponse, error) {
	resp := &hostv1.ApplyImportResponse{}
	for _, p := range plans {
		doc, ok := s.docs.getLocked(envCollection, p.name)
		if !ok {
			return nil, status.Errorf(codes.Internal, "imported environment %s vanished immediately", quoteName(p.name))
		}
		env, err := environmentFromDoc(doc)
		if err != nil {
			return nil, err
		}
		resp.Environments = append(resp.Environments, cloneEnvironment(env))
	}
	return resp, nil
}
