package code

// This file is the import scaffolding the lenient parsers share. Like the
// rest of the package it is pure: it depends on the generated hostv1 types,
// protobuf and the standard library, never on approval, host or host/local.
// A host or pack composes those at its own call site and adapts its git repo
// onto ImportFS.

import (
	"unicode/utf8"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// ImportFile is one regular file in a branch snapshot, as ImportFS lists it.
type ImportFile struct {
	// Path is repo-relative with forward slashes.
	Path string
	// Mode is the git file mode as an octal string, for example "100644".
	Mode string
	// Size is the blob size in bytes.
	Size int64
}

// ImportFS is the pure format layer's only view of a branch's files: a list
// and a read. The host adapts its git repository onto it, so this package
// needs no git binary and no subprocess to be tested, and no git or gRPC
// type crosses into it.
type ImportFS interface {
	// List returns the regular files matching the given pathspecs.
	List(pathspecs ...string) ([]ImportFile, error)
	// Read returns the contents of one file by repo-relative path.
	Read(path string) ([]byte, error)
}

// Documented defaults for ImportLimits (DQ-14, DQ-8).
const (
	DefaultMaxFileBytes         int64 = 1 << 20
	DefaultMaxBranchBytes       int64 = 4 << 20
	DefaultMaxSnapshotBytes     int64 = 8 << 20
	DefaultExcerptBytes               = 200
	DefaultMaxFindingsPerBranch       = 200
)

// ImportLimits holds every size and count ceiling the import path enforces.
// Each is a field so a test can shrink one without changing production
// behaviour; none is read from a literal at a use site.
type ImportLimits struct {
	// MaxFileBytes is the per-file ceiling (D-04).
	MaxFileBytes int64
	// MaxBranchBytes is the per-branch data ceiling (D-04).
	MaxBranchBytes int64
	// MaxSnapshotBytes is the total snapshot ceiling (D-12, DQ-8).
	MaxSnapshotBytes int64
	// ExcerptBytes caps a finding's raw excerpt (Pitfall 9).
	ExcerptBytes int
	// MaxFindingsPerBranch caps findings per branch; one terminal finding
	// is appended past the cap (Pitfall 9).
	MaxFindingsPerBranch int
}

// DefaultImportLimits returns the production limits.
func DefaultImportLimits() ImportLimits {
	return ImportLimits{
		MaxFileBytes:         DefaultMaxFileBytes,
		MaxBranchBytes:       DefaultMaxBranchBytes,
		MaxSnapshotBytes:     DefaultMaxSnapshotBytes,
		ExcerptBytes:         DefaultExcerptBytes,
		MaxFindingsPerBranch: DefaultMaxFindingsPerBranch,
	}
}

// withDefaults fills any zero field from the defaults.
func (l ImportLimits) withDefaults() ImportLimits {
	return l
}

// Finding kinds. There are exactly two severities in use (D-10): a warning
// means the construct was skipped and the file was otherwise imported; an
// error means the file is unparseable and absent from the snapshot. Nothing
// in the Puppetfile catalog is an error.
const (
	// FindingPuppetfileForgeDirective: warning; the directive line is skipped.
	FindingPuppetfileForgeDirective = "puppetfile_forge_directive"
	// FindingPuppetfileUnmodelledAttribute: warning; the whole mod statement is skipped.
	FindingPuppetfileUnmodelledAttribute = "puppetfile_unmodelled_attribute"
	// FindingPuppetfileUnsupportedRuby: warning; the logical line is skipped.
	FindingPuppetfileUnsupportedRuby = "puppetfile_unsupported_ruby"
	// FindingPuppetfileConflictingRef: warning; the whole mod statement is skipped.
	FindingPuppetfileConflictingRef = "puppetfile_conflicting_ref"
	// FindingPuppetfileDuplicateModule: warning; the first occurrence is kept.
	FindingPuppetfileDuplicateModule = "puppetfile_duplicate_module"
	// FindingPuppetfileDuplicateModuledir: warning; the last value is kept.
	FindingPuppetfileDuplicateModuledir = "puppetfile_duplicate_moduledir"
	// FindingPuppetfileInvalidModule: warning; the module is skipped.
	FindingPuppetfileInvalidModule = "puppetfile_invalid_module"
	// FindingFindingsTruncated: warning; terminal, appended once past the cap.
	FindingFindingsTruncated = "findings_truncated"
	// FindingBranchNameInvalid: branch-level, for the environment-name rule (D-14).
	FindingBranchNameInvalid = "branch_name_invalid"
)

// truncateExcerpt is a stub in the RED commit.
func truncateExcerpt(s string, max int) string {
	_ = utf8.RuneError
	return s
}

// newFinding builds one finding with a capped, valid-UTF-8 excerpt.
func newFinding(kind string, sev hostv1.ImportFinding_Severity, line int, excerpt, message string, lim ImportLimits) *hostv1.ImportFinding {
	return &hostv1.ImportFinding{
		Kind:     kind,
		Severity: sev,
		Line:     int32(line),
		Excerpt:  excerpt,
		Message:  message,
	}
}

// findingList accumulates findings under the per-branch cap.
type findingList struct {
	lim   ImportLimits
	items []*hostv1.ImportFinding
}

func newFindingList(lim ImportLimits) *findingList { return &findingList{lim: lim} }

// add appends f.
func (l *findingList) add(f *hostv1.ImportFinding) { l.items = append(l.items, f) }

// stampFindings sets branch and file on every finding.
func stampFindings(fs []*hostv1.ImportFinding, branch, file string) {}
