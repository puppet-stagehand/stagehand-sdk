package code

// This file is the import scaffolding the lenient parsers share. Like the
// rest of the package it is pure: it depends on the generated hostv1 types,
// protobuf and the standard library, never on approval, host or host/local.
// A host or pack composes those at its own call site and adapts its git repo
// onto ImportFS.

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

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
	d := DefaultImportLimits()
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxBranchBytes <= 0 {
		l.MaxBranchBytes = d.MaxBranchBytes
	}
	if l.MaxSnapshotBytes <= 0 {
		l.MaxSnapshotBytes = d.MaxSnapshotBytes
	}
	if l.ExcerptBytes <= 0 {
		l.ExcerptBytes = d.ExcerptBytes
	}
	if l.MaxFindingsPerBranch <= 0 {
		l.MaxFindingsPerBranch = d.MaxFindingsPerBranch
	}
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

// Hiera finding kinds (IMP-03). The version and unparseable kinds are errors
// and leave the hierarchy out of the snapshot; the rest are warnings.
const (
	// FindingHieraVersionUnsupported: error; the hierarchy is absent. The
	// strict parser accepts a Hiera v3 file as an empty hierarchy, which would
	// be written over a real one, so the version is checked explicitly.
	FindingHieraVersionUnsupported = "hiera_version_unsupported"
	// FindingHieraUnparseable: error; the hierarchy is absent because the
	// facet's own read path could not read it back.
	FindingHieraUnparseable = "hiera_unparseable"
	// FindingHieraUnmodelledKey: warning; the key is kept in the stored text
	// but the Code model has no field for it.
	FindingHieraUnmodelledKey = "hiera_unmodelled_key"
	// FindingHieraLevelUnnamed: warning; levels are matched by name.
	FindingHieraLevelUnnamed = "hiera_level_unnamed"
	// FindingHieraDuplicateLevel: warning; levels are matched by name.
	FindingHieraDuplicateLevel = "hiera_duplicate_level"
	// FindingHieraDatadirUnresolvable: warning; no files are imported for the
	// level (or defaults) that declared the datadir.
	FindingHieraDatadirUnresolvable = "hiera_datadir_unresolvable"
)

// Data-file finding kinds (IMP-03, D-12). Only the unparseable kind is an
// error; every other one skips a file with a warning.
const (
	// FindingDataFileUnparseable: error; the facet could not read the file
	// back, so it is absent from the snapshot.
	FindingDataFileUnparseable = "data_file_unparseable"
	// FindingDataFileNotYAML: warning; not imported.
	FindingDataFileNotYAML = "data_file_not_yaml"
	// FindingDataFileSymlink: warning; never read through.
	FindingDataFileSymlink = "data_file_symlink"
	// FindingDataFileGitlink: warning; never read through.
	FindingDataFileGitlink = "data_file_gitlink"
	// FindingDataFileCollision: warning; the first file in hierarchy order is kept.
	FindingDataFileCollision = "data_file_collision"
	// FindingDataFileTooLarge: warning; not read.
	FindingDataFileTooLarge = "data_file_too_large"
	// FindingDataFileExtraDocuments: warning; only the first document counts.
	FindingDataFileExtraDocuments = "data_file_extra_documents"
	// FindingDataFileOutsideDatadir: warning; not imported.
	FindingDataFileOutsideDatadir = "data_file_outside_datadir"
)

// environment.conf finding kinds (IMP-04). All are warnings: the file is
// never unparseable as a whole, the offending line is skipped.
const (
	// FindingEnvConfUnrecognizedKey: an unknown key.
	FindingEnvConfUnrecognizedKey = "envconf_unrecognized_key"
	// FindingEnvConfMalformedLine: a line with no equals sign (a section
	// header is one), or a value that cannot be stored faithfully.
	FindingEnvConfMalformedLine = "envconf_malformed_line"
	// FindingEnvConfInvalidBoolean: a boolean key with a non-boolean value.
	FindingEnvConfInvalidBoolean = "envconf_invalid_boolean"
)

// Branch-level finding kinds raised by AnalyzeBranch itself.
const (
	// FindingBranchDataCapExceeded: warning; the data-file walk stopped at a
	// byte budget and the files already imported are kept.
	FindingBranchDataCapExceeded = "branch_data_cap_exceeded"
	// FindingBranchFileUnreadable: error; one of the three root files
	// (Puppetfile, hiera.yaml, environment.conf) is a symlink, a gitlink,
	// over the per-file cap, unreadable, or failed the facet's own read path,
	// so it is absent from the snapshot. Not in the plan's catalog: the
	// catalog gives the three root files no kind for these cases.
	FindingBranchFileUnreadable = "branch_file_unreadable"
)

// truncateExcerpt returns s made valid UTF-8 and cut to at most max bytes on
// a rune boundary. An invalid sequence cannot be held by a structpb.Struct
// and would fail the proposal write late (T-10-39), so the result is always
// valid, whatever the remote bytes were.
func truncateExcerpt(s string, max int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// newFinding builds one finding with an excerpt capped at lim.ExcerptBytes
// on a rune boundary. Branch and file are left empty: a parser does not know
// which branch it serves, and stampFindings sets both (D-10).
func newFinding(kind string, sev hostv1.ImportFinding_Severity, line int, excerpt, message string, lim ImportLimits) *hostv1.ImportFinding {
	lim = lim.withDefaults()
	return &hostv1.ImportFinding{
		Kind:     kind,
		Severity: sev,
		Line:     int32(line),
		Excerpt:  truncateExcerpt(excerpt, lim.ExcerptBytes),
		Message:  message,
	}
}

// findingList accumulates findings under the per-branch cap (T-10-17).
type findingList struct {
	lim       ImportLimits
	items     []*hostv1.ImportFinding
	truncated bool
}

func newFindingList(lim ImportLimits) *findingList {
	return &findingList{lim: lim.withDefaults()}
}

// add appends f while under lim.MaxFindingsPerBranch. The first finding past
// the cap is replaced by one terminal truncation finding carrying its
// line, and every later one is dropped, so the list holds at most cap+1
// entries and the loss is never silent.
func (l *findingList) add(f *hostv1.ImportFinding) {
	if l.truncated {
		return
	}
	if len(l.items) >= l.lim.MaxFindingsPerBranch {
		l.truncated = true
		line := int(f.GetLine())
		if line < 1 {
			line = 1
		}
		l.items = append(l.items, newFinding(FindingFindingsTruncated, hostv1.ImportFinding_WARNING, line, "",
			fmt.Sprintf("more than %d findings in this branch; the rest are not listed (the findings cap, never a silent drop)", l.lim.MaxFindingsPerBranch), l.lim))
		return
	}
	l.items = append(l.items, f)
}

// stampFindings sets branch and file on every finding, so a parser can stay
// ignorant of which branch it is serving and AnalyzeBranch stamps once.
func stampFindings(fs []*hostv1.ImportFinding, branch, file string) {
	for _, f := range fs {
		f.Branch = branch
		f.File = file
	}
}

// The three root files AnalyzeBranch reads. The branch root is the
// environment root: r10k's branch-equals-environment model (D-13).
const (
	branchPuppetfile  = "Puppetfile"
	branchHieraYaml   = "hiera.yaml"
	branchEnvironConf = "environment.conf"
)

// defaultDatadir is Hiera's default datadir, next to hiera.yaml.
const defaultDatadir = "data"

// Git file modes AnalyzeBranch refuses to read through.
const (
	modeSymlink = "120000"
	modeGitlink = "160000"
)

// branchAnalyzer holds one AnalyzeBranch run's state.
type branchAnalyzer struct {
	branch string
	fs     ImportFS
	lim    ImportLimits
	fl     *findingList
	snap   *hostv1.ImportBranchSnapshot
}

// add stamps fs with the branch and file and adds each under the branch's
// findings cap.
func (a *branchAnalyzer) add(file string, fs []*hostv1.ImportFinding) {
	stampFindings(fs, a.branch, file)
	for _, f := range fs {
		a.fl.add(f)
	}
}

// fail records one error finding for file: the file is absent from the
// snapshot.
func (a *branchAnalyzer) fail(file, kind, msg string) {
	a.add(file, []*hostv1.ImportFinding{newFinding(kind, hostv1.ImportFinding_ERROR, 0, "", msg, a.lim)})
}

// warn records one warning finding for file.
func (a *branchAnalyzer) warn(file, kind, msg string) {
	a.add(file, []*hostv1.ImportFinding{newFinding(kind, hostv1.ImportFinding_WARNING, 0, "", msg, a.lim)})
}

// AnalyzeBranch turns one branch's files into the snapshot a human reviews
// and ApplyImport writes, plus the findings (IMP-03, IMP-04). It reads only
// through fs, so every policy decision about which files matter is testable
// without git. It reads Puppetfile, hiera.yaml and environment.conf at the
// branch root, resolves the declared datadirs from the hierarchy, and imports
// the data files under them. It lists nothing else, so module-level Hiera
// configuration, which is out of scope, is never reached.
//
// Every text it puts in the snapshot has already passed the Code facet's own
// read path for the collection it will be stored in: the Puppetfile text
// re-parses through strict ParsePuppetfile to an equal model, the hierarchy
// text through ParseHierarchy, each data file through ValidateDataPath and
// ParseDataFile, and the settings through RenderEnvConf and ParseEnvConf. A
// text that fails its own read path is an error finding and is absent, never
// silently stored; that is what lets ApplyImport validate in one pass and
// write in an infallible second one.
//
// Every finding is stamped with branch and, where file-scoped, the
// repo-relative file. The same findings are returned and set on the snapshot.
func AnalyzeBranch(branch string, fs ImportFS, lim ImportLimits) (*hostv1.ImportBranchSnapshot, []*hostv1.ImportFinding) {
	lim = lim.withDefaults()
	a := &branchAnalyzer{
		branch: branch,
		fs:     fs,
		lim:    lim,
		fl:     newFindingList(lim),
		snap:   &hostv1.ImportBranchSnapshot{Branch: branch, Importable: true},
	}

	roots, err := fs.List(branchPuppetfile, branchHieraYaml, branchEnvironConf)
	if err != nil {
		a.fail("", FindingBranchFileUnreadable, fmt.Sprintf("the branch's files could not be listed, so nothing was imported: %v", err))
		a.snap.Importable = false
		return a.finish()
	}
	byPath := map[string]ImportFile{}
	for _, f := range roots {
		byPath[f.Path] = f
	}

	if text, ok := a.readRoot(byPath, branchPuppetfile); ok {
		a.importPuppetfile(text)
	}
	var hier *hostv1.HieraHierarchy
	hieraUsable := true
	if text, ok := a.readRoot(byPath, branchHieraYaml); ok {
		hier, hieraUsable = a.importHiera(text)
	} else if _, present := byPath[branchHieraYaml]; present {
		hieraUsable = false
	}
	if text, ok := a.readRoot(byPath, branchEnvironConf); ok {
		a.importEnvConf(text)
	}
	if hieraUsable {
		a.importData(hier)
	}
	return a.finish()
}

// finish stamps the terminal truncation finding, which no parser built, and
// attaches the findings to the snapshot.
func (a *branchAnalyzer) finish() (*hostv1.ImportBranchSnapshot, []*hostv1.ImportFinding) {
	for _, f := range a.fl.items {
		if f.Branch == "" {
			f.Branch = a.branch
		}
	}
	a.snap.Findings = a.fl.items
	return a.snap, a.fl.items
}

// readRoot reads one root file. It reports false, with no finding, when the
// file is absent; and false with an error finding when it is present but
// cannot be read: a symlink or gitlink is never read through, and the size is
// checked from the size ImportFS reports, before any read.
func (a *branchAnalyzer) readRoot(byPath map[string]ImportFile, name string) (string, bool) {
	e, ok := byPath[name]
	if !ok {
		return "", false
	}
	switch e.Mode {
	case modeSymlink:
		a.fail(name, FindingBranchFileUnreadable, name+" is a symlink and is never read through, so it was not imported")
		return "", false
	case modeGitlink:
		a.fail(name, FindingBranchFileUnreadable, name+" is a gitlink (a submodule), not a file, so it was not imported")
		return "", false
	}
	if e.Size > a.lim.MaxFileBytes {
		a.fail(name, FindingBranchFileUnreadable, fmt.Sprintf("%s is %d bytes, over the per-file cap of %d bytes (ImportLimits.MaxFileBytes), so it was not read or imported", name, e.Size, a.lim.MaxFileBytes))
		return "", false
	}
	b, err := a.fs.Read(name)
	if err != nil {
		a.fail(name, FindingBranchFileUnreadable, fmt.Sprintf("%s could not be read, so it was not imported: %v", name, err))
		return "", false
	}
	if int64(len(b)) > a.lim.MaxFileBytes {
		a.fail(name, FindingBranchFileUnreadable, fmt.Sprintf("%s is %d bytes, over the per-file cap of %d bytes (ImportLimits.MaxFileBytes), so it was not imported", name, len(b), a.lim.MaxFileBytes))
		return "", false
	}
	return string(b), true
}

// puppetfileReadPath renders pf and requires strict ParsePuppetfile to read
// the rendered text back to an equal model. It returns the text to store.
func puppetfileReadPath(pf *hostv1.Puppetfile) (string, error) {
	text, err := RenderPuppetfile(pf)
	if err != nil {
		return "", fmt.Errorf("the model does not render: %w", err)
	}
	back, err := ParsePuppetfile(text)
	if err != nil {
		return "", fmt.Errorf("the rendered text does not parse strictly: %w", err)
	}
	if !proto.Equal(back, pf) {
		return "", fmt.Errorf("the rendered text parses strictly to a different model")
	}
	return text, nil
}

func (a *branchAnalyzer) importPuppetfile(text string) {
	pf, fs := ParsePuppetfileLenient(text, a.lim)
	a.add(branchPuppetfile, fs)
	rendered, err := puppetfileReadPath(pf)
	if err != nil {
		a.fail(branchPuppetfile, FindingBranchFileUnreadable, fmt.Sprintf("the Puppetfile failed the Code facet's own read path, so it was not imported: %v", err))
		return
	}
	a.snap.PuppetfileText = rendered
}

// hierarchyReadPath requires strict ParseHierarchy to read the retained text
// back to the model the lenient parse produced.
func hierarchyReadPath(raw string, h *hostv1.HieraHierarchy) error {
	back, err := ParseHierarchy(raw)
	if err != nil {
		return err
	}
	if !proto.Equal(back, h) {
		return fmt.Errorf("the retained text reads back as a different hierarchy")
	}
	return nil
}

// importHiera returns the hierarchy and whether it is usable: an unusable one
// is absent from the snapshot and its datadirs cannot be known, so no data
// files are imported.
func (a *branchAnalyzer) importHiera(text string) (*hostv1.HieraHierarchy, bool) {
	h, raw, fs, ok := ParseHierarchyLenient(text, a.lim)
	a.add(branchHieraYaml, fs)
	if !ok {
		return nil, false
	}
	if err := hierarchyReadPath(raw, h); err != nil {
		a.fail(branchHieraYaml, FindingBranchFileUnreadable, fmt.Sprintf("hiera.yaml failed the Code facet's own read path, so it was not imported: %v", err))
		return nil, false
	}
	a.snap.HieraYaml = raw
	return h, true
}

// settingsReadPath requires RenderEnvConf then ParseEnvConf to give the
// settings back unchanged: the equivalent of the host's own round-trip check,
// run in the pure layer so that check is a confirmation, never the first
// rejection.
func settingsReadPath(s *hostv1.EnvironmentSettings) error {
	text, err := RenderEnvConf(s)
	if err != nil {
		return err
	}
	back, err := ParseEnvConf(text)
	if err != nil {
		return err
	}
	if !proto.Equal(back, s) {
		return fmt.Errorf("the settings read back different after a render and parse")
	}
	return nil
}

func (a *branchAnalyzer) importEnvConf(text string) {
	s, fs := ParseEnvConfLenient(text, a.lim)
	a.add(branchEnvironConf, fs)
	if s == nil {
		return
	}
	if err := settingsReadPath(s); err != nil {
		a.fail(branchEnvironConf, FindingBranchFileUnreadable, fmt.Sprintf("environment.conf failed the Code facet's own read path, so its settings were not imported: %v", err))
		return
	}
	a.snap.Settings = s
}

// isYAMLPath reports whether p carries a YAML extension. Hiera data files are
// named .yaml, and .yml is the same format.
func isYAMLPath(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// importData imports the data files under the datadirs the hierarchy
// declares, each under its datadir-relative path. Each candidate is checked
// in a fixed order, cheapest and safest first, so a file is never read before
// it is known to be wanted: scope (inside a declared datadir), mode (a
// symlink or gitlink is never read through), extension, the reported size
// against the per-file cap, collision with a file already imported, and only
// then the read and the facet's own parser (T-10-21).
func (a *branchAnalyzer) importData(h *hostv1.HieraHierarchy) {
	dirs := effectiveDatadirs(h)
	if len(dirs) == 0 {
		return
	}
	entries, err := a.fs.List(dirs...)
	if err != nil {
		a.fail("", FindingBranchFileUnreadable, fmt.Sprintf("the data directories could not be listed: %v", err))
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	// Each entry belongs to the first declared datadir that contains it, so
	// the walk below runs in hierarchy order and a collision keeps the
	// earliest level's file.
	owned := make([][]ImportFile, len(dirs))
	for _, e := range entries {
		placed := false
		for i, dd := range dirs {
			if strings.HasPrefix(e.Path, dd+"/") {
				owned[i] = append(owned[i], e)
				placed = true
				break
			}
		}
		if !placed && e.Path != branchPuppetfile && e.Path != branchHieraYaml && e.Path != branchEnvironConf {
			a.warn(e.Path, FindingDataFileOutsideDatadir, "the file is outside every declared datadir, so it was not imported")
		}
	}

	claimed := map[string]string{} // relative path -> repo path imported under it
	for i, dd := range dirs {
		for _, e := range owned[i] {
			a.importDataFile(dd, e, claimed)
		}
	}
}

// importDataFile applies the per-file checks to one entry under datadir dd.
func (a *branchAnalyzer) importDataFile(dd string, e ImportFile, claimed map[string]string) {
	switch e.Mode {
	case modeSymlink:
		a.warn(e.Path, FindingDataFileSymlink, "the entry is a symlink and is never read through, so it was not imported")
		return
	case modeGitlink:
		a.warn(e.Path, FindingDataFileGitlink, "the entry is a gitlink (a submodule), not a file, so it was not imported")
		return
	}
	if !isYAMLPath(e.Path) {
		a.warn(e.Path, FindingDataFileNotYAML, "the file is not YAML (only .yaml and .yml are imported), so it was not imported")
		return
	}
	if e.Size > a.lim.MaxFileBytes {
		a.warn(e.Path, FindingDataFileTooLarge, fmt.Sprintf("the file is %d bytes, over the per-file cap of %d bytes (ImportLimits.MaxFileBytes), so it was not read or imported", e.Size, a.lim.MaxFileBytes))
		return
	}
	rel := strings.TrimPrefix(e.Path, dd+"/")
	if first, dup := claimed[rel]; dup {
		a.warn(e.Path, FindingDataFileCollision, fmt.Sprintf("the datadir-relative path %q is already imported from %s and the first file in hierarchy order is kept; the Code model stores a data file by a path relative to the datadir and has no datadir dimension, so this second file cannot be represented", rel, first))
		return
	}
	b, err := a.fs.Read(e.Path)
	if err != nil {
		a.fail(e.Path, FindingDataFileUnparseable, fmt.Sprintf("the file could not be read, so it was not imported: %v", err))
		return
	}
	if int64(len(b)) > a.lim.MaxFileBytes {
		a.warn(e.Path, FindingDataFileTooLarge, fmt.Sprintf("the file is %d bytes, over the per-file cap of %d bytes (ImportLimits.MaxFileBytes), so it was not imported", len(b), a.lim.MaxFileBytes))
		return
	}
	df, fs := ParseDataFileLenient(rel, string(b), a.lim)
	a.add(e.Path, fs)
	if df == nil {
		return
	}
	claimed[rel] = e.Path
	a.snap.DataFiles = append(a.snap.DataFiles, df)
}
