package code

// This file is the lenient half of the Hiera import path: the entry points
// that turn a branch's hiera.yaml and data files into snapshot content plus
// findings (IMP-03). Like the rest of the package it is pure: it imports only
// hostv1, yaml and the standard library, never approval, host or host/local.
//
// The strict parsers in hiera.go were written for the write side. They read
// the fields the model has and ignore the rest, so on a real control-repo
// file they fail silently at least as often as loudly (RESEARCH Pitfall 1):
// a Hiera v3 file parses as an empty hierarchy, an eyaml level loses its
// backend, a globs level survives as a nameless shell. Every one of those is
// an explicit, tested decision here.

import (
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	yaml "go.yaml.in/yaml/v3"
)

// utf8BOM is the byte-order mark the strict parsers reject at the start of a
// file.
const utf8BOM = "\xEF\xBB\xBF"

// supportedHieraVersion is the only hiera.yaml version the Code model reads.
const supportedHieraVersion = 5

// maxDataNestingDepth bounds how deeply a data file's YAML may nest before it
// is refused. The YAML library's own ceiling is 10000, but a structpb value
// costs two protobuf messages per level and the proposal body is later
// marshalled and re-read under protobuf's recursion limit, so a file that
// deep would pass the facet's parser and fail at proposal-write time. No real
// Hiera data nests anywhere near this deep.
const maxDataNestingDepth = 64

// hieraDefaultsKeys and hieraLevelKeys are the keys the Code model carries in
// the defaults mapping and in a level, matching what ParseHierarchy and
// levelFromNode read. They are the same keys, so a modelled key cannot start
// producing false findings (T-10 key link).
var hieraDefaultsKeys = map[string]bool{"datadir": true, "data_hash": true}

var hieraLevelKeys = map[string]bool{
	"name": true, "path": true, "paths": true, "glob": true,
	"mapped_paths": true, "datadir": true, "data_hash": true,
}

// hieraTopLevelKeys is the set of top-level keys the Code model carries.
// Everything else, including the valid-but-unmodelled default_hierarchy and
// the Bolt-only plan_hierarchy, is flagged (D-09).
var hieraTopLevelKeys = map[string]bool{
	"version":   true,
	"defaults":  true,
	"hierarchy": true,
}

// ParseHierarchyLenient turns one branch's hiera.yaml text into the model,
// the raw text to store, the findings, and whether the file is importable at
// all. The raw text is the input with any BOM removed: it is stored verbatim
// whenever strict ParseHierarchy accepts it (DQ-4), so an eyaml level keeps
// its lookup_key and options in the stored file.
//
// A file is not importable, and is absent from the snapshot, when it cannot
// be decoded, when its version is anything but 5, or when the strict parser
// refuses it. The version is checked explicitly and before anything else is
// read because the strict parser accepts a v3 file as a hierarchy with no
// version and no levels, and an empty hierarchy written over a real one is
// data loss (T-10-19).
func ParseHierarchyLenient(yamlText string, lim ImportLimits) (*hostv1.HieraHierarchy, string, []*hostv1.ImportFinding, bool) {
	lim = lim.withDefaults()
	yamlText = strings.TrimPrefix(yamlText, utf8BOM)
	fl := newFindingList(lim)
	failed := func(kind string, line int, excerpt, msg string) (*hostv1.HieraHierarchy, string, []*hostv1.ImportFinding, bool) {
		fl.add(newFinding(kind, hostv1.ImportFinding_ERROR, line, excerpt, msg, lim))
		return nil, "", fl.items, false
	}

	doc, err := decodeDoc(yamlText)
	if err != nil {
		return failed(FindingHieraUnparseable, 0, "",
			fmt.Sprintf("hiera.yaml cannot be read as a YAML mapping, so the Code facet could not read it back and it was not imported: %v", err))
	}
	if doc == nil {
		return failed(FindingHieraVersionUnsupported, 0, "",
			"hiera.yaml has no content and so no version key; only Hiera version 5 is supported, and an empty hierarchy is never imported over a real one")
	}
	m := rootMapping(doc)

	// The explicit version check. It comes first after the decode: a v3
	// file's remaining keys mean nothing under the v5 model, and walking
	// them would bury the one real error under a cloud of unmodelled-key
	// noise.
	vn := mapValue(m, "version")
	if vn == nil {
		return failed(FindingHieraVersionUnsupported, firstKeyLine(m), firstKeyText(m), versionMessage(m, nil))
	}
	var version int
	if vn.Kind != yaml.ScalarNode || vn.ShortTag() != "!!int" || vn.Decode(&version) != nil || version != supportedHieraVersion {
		return failed(FindingHieraVersionUnsupported, vn.Line, vn.Value, versionMessage(m, vn))
	}

	h, err := ParseHierarchy(yamlText)
	if err != nil {
		return failed(FindingHieraUnparseable, 0, "",
			fmt.Sprintf("hiera.yaml does not have the shape the Code facet reads, so it could not be read back and was not imported: %v", err))
	}

	warn := func(kind string, line int, excerpt, msg string) {
		fl.add(newFinding(kind, hostv1.ImportFinding_WARNING, line, excerpt, msg, lim))
	}
	unmodelled := func(k *yaml.Node, scope, rewrite string) {
		warn(FindingHieraUnmodelledKey, k.Line, k.Value, fmt.Sprintf(
			"%s key %q is kept in the stored hiera.yaml text but the Code model has no field for it; %s", scope, k.Value, rewrite))
	}

	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if !hieraTopLevelKeys[k.Value] {
			unmodelled(k, "top-level", "the facet cannot show or edit it")
		}
	}

	if defaults := mapValue(m, "defaults"); defaults != nil {
		if defaults.Kind != yaml.MappingNode {
			warn(FindingHieraUnmodelledKey, defaults.Line, "defaults",
				"defaults is not a mapping, so the Code model reads nothing from it; it is kept in the stored text")
		} else {
			for i := 0; i+1 < len(defaults.Content); i += 2 {
				k := defaults.Content[i]
				if !hieraDefaultsKeys[k.Value] {
					unmodelled(k, "defaults", "the facet cannot show or edit it")
				}
			}
			if dn := mapValue(defaults, "datadir"); dn != nil {
				if _, problem := cleanDatadir(dn.Value); problem != "" {
					warn(FindingHieraDatadirUnresolvable, dn.Line, dn.Value, fmt.Sprintf(
						"defaults.datadir %q %s; levels that inherit it import no data files", dn.Value, problem))
				}
			}
		}
	}

	if seq := mapValue(m, "hierarchy"); seq != nil {
		seen := map[string]bool{}
		for idx, ln := range seq.Content {
			name := ""
			if nn := mapValue(ln, "name"); nn != nil {
				name = nn.Value
			}
			label := fmt.Sprintf("level %q", name)
			switch {
			case name == "":
				label = fmt.Sprintf("level #%d", idx+1)
				warn(FindingHieraLevelUnnamed, ln.Line, "", fmt.Sprintf(
					"%s has no name; PutHieraLevel, RemoveHieraLevel and ReorderHieraLevels match levels by name, so the facet cannot address it", label))
			case seen[name]:
				warn(FindingHieraDuplicateLevel, ln.Line, name, fmt.Sprintf(
					"the name %q is used by more than one level; PutHieraLevel, RemoveHieraLevel and ReorderHieraLevels match levels by name, so only the first can be addressed", name))
			}
			if name != "" {
				seen[name] = true
			}
			rewrite := "a later PutHieraLevel rewrite of this level rebuilds it from the model and would drop it"
			for j := 0; j+1 < len(ln.Content); j += 2 {
				k := ln.Content[j]
				if !hieraLevelKeys[k.Value] {
					unmodelled(k, label, rewrite)
				}
			}
			if dn := mapValue(ln, "datadir"); dn != nil {
				if _, problem := cleanDatadir(dn.Value); problem != "" {
					warn(FindingHieraDatadirUnresolvable, dn.Line, dn.Value, fmt.Sprintf(
						"%s declares datadir %q which %s; no data files were imported for it", label, dn.Value, problem))
				}
			}
		}
	}
	return h, yamlText, fl.items, true
}

// firstKeyLine and firstKeyText locate the first top-level key, for the
// version finding when there is no version key to point at.
func firstKeyLine(m *yaml.Node) int {
	if m != nil && len(m.Content) > 0 {
		return m.Content[0].Line
	}
	return 0
}

func firstKeyText(m *yaml.Node) string {
	if m != nil && len(m.Content) > 0 {
		return m.Content[0].Value
	}
	return ""
}

// versionMessage explains an unsupported or missing version. A first key that
// starts with a colon is the Hiera v3 shape (":backends:", ":hierarchy:").
func versionMessage(m *yaml.Node, vn *yaml.Node) string {
	v3 := ""
	if m != nil {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if strings.HasPrefix(m.Content[i].Value, ":") {
				v3 = " It looks like a Hiera v3 file (colon-prefixed keys such as :backends: and :hierarchy:)."
				break
			}
		}
	}
	if vn == nil {
		return "hiera.yaml has no version key; only Hiera version 5 is supported, so it was not imported." + v3 +
			" Importing it as an empty hierarchy would silently overwrite a real one."
	}
	return fmt.Sprintf("hiera.yaml declares version %q; only Hiera version 5 is supported, so it was not imported.", vn.Value) + v3 +
		" Importing it as an empty hierarchy would silently overwrite a real one."
}

// cleanDatadir normalises a declared datadir and says why it cannot be
// followed. A datadir that interpolates (%{...}) depends on facts and cannot
// be resolved statically; an absolute path or one with a ".." segment would
// leave the environment (T-10-42); one that resolves to the environment root
// would sweep in every YAML file the branch holds; and one under modules/ or
// site-modules/ would walk a module tree, which is out of scope. The returned
// problem is empty when the datadir is usable, and the cleaned path is
// relative with no "./" prefix or trailing slash.
func cleanDatadir(dd string) (string, string) {
	switch {
	case strings.Contains(dd, "%{"):
		return "", "interpolates a variable and cannot be resolved without facts"
	case strings.HasPrefix(dd, "/"):
		return "", "is an absolute path"
	case strings.ContainsAny(dd, "\\\x00\n"):
		return "", "contains a backslash, NUL or newline"
	}
	for _, seg := range strings.Split(dd, "/") {
		if seg == ".." {
			return "", `contains a ".." segment`
		}
	}
	c := path.Clean(dd)
	switch {
	case c == ".":
		return "", "resolves to the environment root"
	case c == "modules" || strings.HasPrefix(c, "modules/") || c == "site-modules" || strings.HasPrefix(c, "site-modules/"):
		return "", "is inside a module tree, which is out of scope"
	}
	return c, ""
}

// effectiveDatadirs lists the datadirs a hierarchy reads, in hierarchy order
// and without duplicates: each level's own datadir, else the hierarchy's
// default, else Hiera's own default. A datadir that cannot be followed is
// skipped; ParseHierarchyLenient has already reported it. A nil hierarchy
// (no hiera.yaml) or one with no levels reads the default datadir, so a
// branch's data files are not silently lost.
func effectiveDatadirs(h *hostv1.HieraHierarchy) []string {
	def := h.GetDefaultDatadir()
	if def == "" {
		def = defaultDatadir
	}
	var out []string
	seen := map[string]bool{}
	add := func(dd string) {
		c, problem := cleanDatadir(dd)
		if problem == "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	if len(h.GetLevels()) == 0 {
		add(def)
		return out
	}
	for _, l := range h.GetLevels() {
		dd := l.GetDatadir()
		if dd == "" {
			dd = def
		}
		add(dd)
	}
	return out
}

// ParseDataFileLenient is the gate every imported data file passes: the path
// must satisfy ValidateDataPath, the text must be valid UTF-8 (a
// structpb.Struct cannot hold anything else, and the failure would otherwise
// surface at proposal-write time instead of as a finding), and strict
// ParseDataFile must accept it — the facet's own read path. A file that fails
// any of them yields one data_file_unparseable error finding naming the cause
// and is absent from the snapshot.
//
// dataPath is the datadir-relative path the file will be stored under. The
// returned finding is unstamped.
func ParseDataFileLenient(dataPath, yamlText string, lim ImportLimits) (*hostv1.ImportDataFile, []*hostv1.ImportFinding) {
	lim = lim.withDefaults()
	reject := func(msg string) (*hostv1.ImportDataFile, []*hostv1.ImportFinding) {
		return nil, []*hostv1.ImportFinding{newFinding(FindingDataFileUnparseable, hostv1.ImportFinding_ERROR, 0, "", msg, lim)}
	}
	if !utf8.ValidString(yamlText) {
		return reject("the file is not valid UTF-8, which the Code facet cannot store, so it was not imported")
	}
	if err := ValidateDataPath(dataPath); err != nil {
		return reject(fmt.Sprintf("the path is not a valid data path, so it was not imported: %v", err))
	}
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return reject(dataFileCause(err))
	}
	if d := nestingDepth(doc); d > maxDataNestingDepth {
		return reject(fmt.Sprintf("the file is nested %d levels deep, past the limit of %d, so it was not imported", d, maxDataNestingDepth))
	}
	if _, err := ParseDataFile(yamlText); err != nil {
		return reject(dataFileCause(err))
	}
	df := &hostv1.ImportDataFile{Path: dataPath, Yaml: yamlText}
	if hasExtraDocuments(yamlText) {
		return df, []*hostv1.ImportFinding{newFinding(FindingDataFileExtraDocuments, hostv1.ImportFinding_WARNING, 0, "",
			"the file holds more than one YAML document; only the first is read, matching the Code facet, and the stored text keeps the rest", lim)}
	}
	return df, nil
}

// nestingDepth is the deepest chain of mappings and sequences under n. Alias
// nodes are not followed: an alias bomb is refused by the YAML library when
// it is decoded, and following aliases here would walk what that refusal
// exists to prevent.
func nestingDepth(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	deepest := 0
	for _, c := range n.Content {
		if d := nestingDepth(c); d > deepest {
			deepest = d
		}
	}
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		return deepest + 1
	}
	return deepest
}

// hasExtraDocuments reports whether text holds a second YAML document with
// content. A trailing bare document marker is not one; a malformed second
// document is, because it is present and ignored.
func hasExtraDocuments(text string) bool {
	dec := yaml.NewDecoder(strings.NewReader(text))
	var first, second yaml.Node
	if err := dec.Decode(&first); err != nil {
		return false
	}
	err := dec.Decode(&second)
	if errors.Is(err, io.EOF) {
		return false
	}
	if err != nil {
		return true
	}
	if second.Kind == yaml.DocumentNode && len(second.Content) == 1 &&
		second.Content[0].Kind == yaml.ScalarNode && second.Content[0].ShortTag() == "!!null" {
		return false
	}
	return second.Kind != 0
}

// dataFileCause names why strict ParseDataFile refused a file, so the finding
// says what to fix instead of repeating a library error.
func dataFileCause(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "expected a YAML mapping document"):
		return "the top level is a sequence or a scalar, not a mapping (a Git LFS pointer file looks like this too), so the Code facet cannot read it back and it was not imported"
	case strings.Contains(msg, "time.Time"):
		return "the file holds an unquoted YAML timestamp or date, which the Code facet cannot represent (quote the value to make it a string), so it was not imported"
	case strings.Contains(msg, "excessive aliasing"):
		return "the file's YAML aliases expand without bound (an alias bomb), so it was not imported"
	case strings.Contains(msg, "max depth"):
		return "the file is nested deeper than the YAML library allows, so it was not imported"
	default:
		return fmt.Sprintf("the Code facet could not read the file back, so it was not imported: %v", err)
	}
}
