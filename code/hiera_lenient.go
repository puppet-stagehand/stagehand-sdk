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
	"fmt"
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

	warn := func(line int, key, msg string) {
		fl.add(newFinding(FindingHieraUnmodelledKey, hostv1.ImportFinding_WARNING, line, key, msg, lim))
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if !hieraTopLevelKeys[k.Value] {
			warn(k.Line, k.Value, fmt.Sprintf(
				"top-level key %q is kept in the stored hiera.yaml text but the Code model has no field for it, so the facet cannot show or edit it", k.Value))
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

// ParseDataFileLenient is the gate every imported data file passes: the path
// must satisfy ValidateDataPath, the text must be valid UTF-8 (a
// structpb.Struct cannot hold anything else, and the failure would otherwise
// surface at proposal-write time instead of as a finding), and strict
// ParseDataFile must accept it — the facet's own read path. A file that fails
// any of them yields one data_file_unparseable error finding naming the cause
// and is absent from the snapshot.
//
// path is the datadir-relative path the file will be stored under. The
// returned finding is unstamped.
func ParseDataFileLenient(path, yamlText string, lim ImportLimits) (*hostv1.ImportDataFile, []*hostv1.ImportFinding) {
	lim = lim.withDefaults()
	reject := func(msg string) (*hostv1.ImportDataFile, []*hostv1.ImportFinding) {
		return nil, []*hostv1.ImportFinding{newFinding(FindingDataFileUnparseable, hostv1.ImportFinding_ERROR, 0, "", msg, lim)}
	}
	if !utf8.ValidString(yamlText) {
		return reject("the file is not valid UTF-8, which the Code facet cannot store, so it was not imported")
	}
	if err := ValidateDataPath(path); err != nil {
		return reject(fmt.Sprintf("the path is not a valid data path, so it was not imported: %v", err))
	}
	if _, err := ParseDataFile(yamlText); err != nil {
		return reject(dataFileCause(err))
	}
	return &hostv1.ImportDataFile{Path: path, Yaml: yamlText}, nil
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
