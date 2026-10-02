// This file implements the Hiera half of the Code facet's file-format
// engine: comment-preserving hiera.yaml hierarchy editing (this file's
// first half) and data file key editing (second half, Task 2), built
// directly on a go.yaml.in/yaml/v3 node tree rather than a struct-tag
// marshaled type, per HIERA-02's structure-preservation requirement.
// The code package's doc comment lives on puppetfile.go; this file
// declares none of its own.
//
// Three bounded facts about the node-tree approach, verified against this
// library version at plan time by a disposable spike (not an assumption)
// — build to them rather than rediscovering them:
//
//  1. The encoder must be configured for two-space indentation
//     (yaml.NewEncoder(...).SetIndent(2)) or it silently reindents every
//     nested line of a round-tripped file from two spaces to four; the
//     package-level marshal helper and a bare default encoder both use
//     four spaces.
//  2. Blank lines and a leading "---" document-start marker are not
//     modelled by the node tree and do not survive a round trip. Comments
//     and key/level order survive; vertical whitespace and the explicit
//     document-start marker do not.
//  3. Scalar style is per-node. A node read from input keeps its original
//     style if left alone. A value this package constructs itself must be
//     given an explicitly quoted style (yaml.DoubleQuotedStyle) whenever
//     it contains an interpolation token (%{...}), begins with a literal
//     '%', or contains a colon-space, because an unquoted value shaped
//     like that is either invalid or fragile YAML.
package code

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	yaml "go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

// ErrHieraParse is wrapped by every error this file returns when input
// text cannot be read as valid YAML, or does not shape a document this
// package understands. host/local/code_hiera.go (Plan 06-07) maps it to
// codes.InvalidArgument via errors.Is, never by string matching.
var ErrHieraParse = errors.New("hiera: parse error")

// ErrHieraInvalid is wrapped by every error this file returns when a
// request is well-formed YAML but semantically invalid for the operation
// requested: an unknown level or key name, a malformed reorder
// permutation, a write to the reserved lookup_options key, or an unsafe
// data-file path.
var ErrHieraInvalid = errors.New("hiera: invalid")

// reEnvInterp matches the %{environment} and %{::environment}
// interpolation forms and nothing else — %{environments} is a different
// fact name and must not match, so the closing brace is anchored
// immediately after the fact name.
var reEnvInterp = regexp.MustCompile(`%\{(?:::)?environment\}`)

// --- node-tree primitives, shared by hierarchy and data-file editing ---

// decodeDoc parses text into its YAML document node. Empty or
// whitespace-only text is not a parse error: it returns (nil, nil),
// because an environment whose hiera.yaml or data file has not been
// authored yet is a legitimate state the facet's read path must be able
// to report. The same holds for a document with no content: a comment-only
// file, a bare "---" and a null root ("~", "null") are the empty state too,
// because a "---"-only or comment-only common.yaml is the commonest real
// placeholder data file in a control repo, and reporting it unparseable
// would produce a false error on a legitimate file (RESEARCH Pitfall 4,
// DQ-6). A non-empty document whose root is not a mapping — a sequence, a
// scalar, a number — is still a parse error, wrapping ErrHieraParse.
func decodeDoc(text string) (*yaml.Node, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHieraParse, err)
	}
	// No content: no document at all (comment-only text), or a document whose
	// whole root is a null scalar (a bare "---", "~", "null"). A quoted "null"
	// is a !!str and falls through to the shape check below.
	if doc.Kind == 0 && len(doc.Content) == 0 {
		return nil, nil
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 &&
		doc.Content[0].Kind == yaml.ScalarNode && doc.Content[0].ShortTag() == "!!null" {
		return nil, nil
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: expected a YAML mapping document", ErrHieraParse)
	}
	return &doc, nil
}

// contentFreePreamble classifies text that decodeDoc reported as having no
// document, and returns the comment preamble a write path must keep above
// whatever it writes. It must be called only for non-blank text for which
// decodeDoc returned no document.
//
// decodeDoc's empty-document relaxation (a comment-only file, a bare "---", a
// null root) is a read-path relaxation: it is what lets a placeholder
// common.yaml import and read without a false error. On a write path the same
// text is a third state, neither "unauthored" (blank) nor "a document": re-encoding
// it from scratch would silently destroy the operator's comments, which is the
// WR-01 regression (Phase 6's HIERA-04 promise that an edit never destroys
// comments, DQ-6-R). So the comment lines are preserved here, and anything the
// helper cannot classify is refused rather than rewritten on a guess.
//
// Every line is exactly one of three things. A blank line or a comment line is
// kept verbatim, which preserves both the comment text and the spacing between
// comments. A line made only of document-start, document-end or null tokens (the
// shapes decodeDoc treats as empty) is dropped, because the written mapping
// replaces it, except that a trailing comment on such a line is kept as its own
// line. Anything else returns an error wrapping ErrHieraInvalid. A leading
// byte-order mark is tolerated and kept. The result is empty, or ends in exactly
// one newline.
func contentFreePreamble(text string) (string, error) {
	const bom = "\ufeff"
	hasBOM := strings.HasPrefix(text, bom)
	text = strings.TrimPrefix(text, bom)

	var kept []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			kept = append(kept, line)
			continue
		}
		rest := trimmed
		for rest != "" {
			if rest[0] == '#' {
				kept = append(kept, rest)
				break
			}
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				end = len(rest)
			}
			switch rest[:end] {
			case "---", "...", "~", "null", "Null", "NULL":
			default:
				return "", fmt.Errorf("%w: the document has comments or markers but no content, and %q cannot be classified as either; the write was refused rather than rewriting the file and dropping it", ErrHieraInvalid, trimmed)
			}
			rest = strings.TrimLeft(rest[end:], " \t")
		}
	}

	out := strings.TrimRight(strings.Join(kept, "\n"), "\n \t")
	if strings.TrimSpace(out) == "" {
		return "", nil
	}
	if hasBOM {
		out = bom + out
	}
	return out + "\n", nil
}

// encodeDoc re-encodes doc (a document node, as returned by decodeDoc or
// built by emptyMappingDoc) to text with two-space indentation at every
// nesting level. The encoder is explicitly closed before the buffer is
// read, which the underlying library requires to flush its final state.
func encodeDoc(doc *yaml.Node) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", fmt.Errorf("%w: %v", ErrHieraInvalid, err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrHieraInvalid, err)
	}
	return buf.String(), nil
}

// rootMapping returns doc's top-level mapping node.
func rootMapping(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

// emptyMappingDoc builds a fresh, empty block-style mapping document. It
// is used as the starting point for a Put against empty input text, in
// preference to parsing a literal "{}" string, which the encoder would
// keep in flow style ("{}") rather than the block style every other
// document in this package emits.
func emptyMappingDoc() *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{m}}
}

// mapValue returns the value node paired with key in mapping node m's
// alternating key/value Content, or nil if m is not a mapping or carries
// no such key.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMapValue replaces the value node paired with key in place when key
// already exists in m, preserving that key's own comments and position:
// the replacement value node inherits the old value node's HeadComment,
// LineComment and FootComment, because a trailing "key: value # comment"
// annotation documents the key, not the specific value, and a caller
// changing only the value should not silently delete it. When key does
// not exist, the pair is appended at the end of m.Content rather than
// sorted into place — a data file's or hierarchy's key/level order
// belongs to the operator or caller, not to this package.
func setMapValue(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			old := m.Content[i+1]
			value.HeadComment = old.HeadComment
			value.LineComment = old.LineComment
			value.FootComment = old.FootComment
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, scalarNode(key, false), value)
}

// deleteMapKey removes the key/value pair named key from mapping node m,
// including any comments attached to either node, and reports whether a
// matching key was found.
func deleteMapKey(m *yaml.Node, key string) bool {
	if m == nil {
		return false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

// scalarNode builds a plain !!str scalar node. quoted forces
// yaml.DoubleQuotedStyle, used for any value whose plain-scalar form
// would be invalid or fragile YAML (see this file's header comment,
// fact 3).
func scalarNode(value string, quoted bool) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	if quoted {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

// needsQuote reports whether s must be written with an explicitly quoted
// style: it contains a Hiera interpolation token, begins with a literal
// '%', or contains a colon-space, any of which is invalid or fragile as a
// plain YAML scalar.
func needsQuote(s string) bool {
	if s == "" {
		return false
	}
	if strings.Contains(s, "%{") {
		return true
	}
	if strings.HasPrefix(s, "%") {
		return true
	}
	if strings.Contains(s, ": ") {
		return true
	}
	return false
}

// strNode builds a !!str scalar node for v, quoting it when needsQuote
// requires it.
func strNode(v string) *yaml.Node {
	return scalarNode(v, needsQuote(v))
}

// seqNode builds a block-style sequence of !!str scalar nodes from items.
func seqNode(items []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, it := range items {
		n.Content = append(n.Content, strNode(it))
	}
	return n
}

// stringSeq reads a sequence node's scalar children as a []string.
func stringSeq(n *yaml.Node) ([]string, error) {
	if n == nil {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%w: expected a YAML sequence", ErrHieraParse)
	}
	out := make([]string, len(n.Content))
	for i, c := range n.Content {
		out[i] = c.Value
	}
	return out, nil
}

// --- hierarchy (hiera.yaml) ---

// ParseHierarchy turns hiera.yaml v5 text into a *hostv1.HieraHierarchy.
// Empty or whitespace-only text is a legitimate unauthored state, not an
// error: it returns a hierarchy with zero levels and a nil error.
// HieraLevel.LookupOptions is left empty here — Plan 06-07 mirrors a data
// file's lookup_options onto a level whose path is literal enough to name
// it without fact interpolation.
func ParseHierarchy(yamlText string) (*hostv1.HieraHierarchy, error) {
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return nil, err
	}
	h := &hostv1.HieraHierarchy{}
	if doc == nil {
		return h, nil
	}
	m := rootMapping(doc)
	if v := mapValue(m, "version"); v != nil {
		if err := v.Decode(&h.Version); err != nil {
			return nil, fmt.Errorf("%w: version: %v", ErrHieraParse, err)
		}
	}
	if defaults := mapValue(m, "defaults"); defaults != nil && defaults.Kind == yaml.MappingNode {
		if dv := mapValue(defaults, "datadir"); dv != nil {
			h.DefaultDatadir = dv.Value
		}
		if dh := mapValue(defaults, "data_hash"); dh != nil {
			h.DefaultDataHash = dh.Value
		}
	}
	if seq := mapValue(m, "hierarchy"); seq != nil {
		if seq.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("%w: hierarchy must be a YAML sequence", ErrHieraParse)
		}
		for _, c := range seq.Content {
			lvl, err := levelFromNode(c)
			if err != nil {
				return nil, err
			}
			h.Levels = append(h.Levels, lvl)
		}
	}
	return h, nil
}

// EmptyHierarchy returns minimal v5 skeleton text: version 5, no
// defaults, no levels. PutLevel starts from this skeleton when its input
// text is empty or whitespace-only.
func EmptyHierarchy() string {
	return "version: 5\n"
}

// levelFromNode reads one hierarchy sequence element into a
// *hostv1.HieraLevel.
func levelFromNode(n *yaml.Node) (*hostv1.HieraLevel, error) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: hierarchy level is not a YAML mapping", ErrHieraParse)
	}
	lvl := &hostv1.HieraLevel{}
	if v := mapValue(n, "name"); v != nil {
		lvl.Name = v.Value
	}
	if v := mapValue(n, "path"); v != nil {
		lvl.Path = v.Value
	}
	if v := mapValue(n, "paths"); v != nil {
		s, err := stringSeq(v)
		if err != nil {
			return nil, err
		}
		lvl.Paths = s
	}
	if v := mapValue(n, "glob"); v != nil {
		lvl.Glob = v.Value
	}
	if v := mapValue(n, "mapped_paths"); v != nil {
		s, err := stringSeq(v)
		if err != nil {
			return nil, err
		}
		lvl.MappedPaths = s
	}
	if v := mapValue(n, "datadir"); v != nil {
		lvl.Datadir = v.Value
	}
	if v := mapValue(n, "data_hash"); v != nil {
		lvl.DataHash = v.Value
	}
	return lvl, nil
}

// levelToNode builds a mapping node for level, emitting only the fields
// that are set, with name first.
func levelToNode(level *hostv1.HieraLevel) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(key string, val *yaml.Node) {
		m.Content = append(m.Content, scalarNode(key, false), val)
	}
	if level.GetName() != "" {
		add("name", strNode(level.GetName()))
	}
	if level.GetPath() != "" {
		add("path", strNode(level.GetPath()))
	}
	if len(level.GetPaths()) > 0 {
		add("paths", seqNode(level.GetPaths()))
	}
	if level.GetGlob() != "" {
		add("glob", strNode(level.GetGlob()))
	}
	if len(level.GetMappedPaths()) > 0 {
		add("mapped_paths", seqNode(level.GetMappedPaths()))
	}
	if level.GetDatadir() != "" {
		add("datadir", strNode(level.GetDatadir()))
	}
	if level.GetDataHash() != "" {
		add("data_hash", strNode(level.GetDataHash()))
	}
	return m
}

// hierarchySeq returns the hierarchy: sequence node of root mapping m,
// creating it (appended at the end of the mapping) if absent.
func hierarchySeq(m *yaml.Node) (*yaml.Node, error) {
	seq := mapValue(m, "hierarchy")
	if seq == nil {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setMapValue(m, "hierarchy", seq)
		return seq, nil
	}
	if seq.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%w: hierarchy must be a YAML sequence", ErrHieraInvalid)
	}
	return seq, nil
}

// levelIndexByName returns the index of the hierarchy sequence element
// whose name key equals name, or -1.
func levelIndexByName(seq *yaml.Node, name string) int {
	for i, c := range seq.Content {
		if nv := mapValue(c, "name"); nv != nil && nv.Value == name {
			return i
		}
	}
	return -1
}

// LevelExists reports whether yamlText's hierarchy holds a level whose name is
// exactly name. It walks the node tree and reads names only, so it does not
// require every other level to convert cleanly: a hierarchy carrying an
// unrelated level ParseHierarchy would reject can still be asked about one
// level. It matches on the same name node PutLevel and RemoveLevel match on, so
// "does it exist" and "would PutLevel replace it" always agree. A hierarchy
// with no hierarchy sequence, or one that is not a sequence, has no level and
// returns false; PutLevel reports the malformed shape itself when it writes.
// Empty or whitespace-only text has no level. An error is returned only when
// yamlText is not a parseable YAML mapping document.
func LevelExists(yamlText, name string) (bool, error) {
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return false, err
	}
	if doc == nil {
		return false, nil
	}
	seq := mapValue(rootMapping(doc), "hierarchy")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return false, nil
	}
	return levelIndexByName(seq, name) >= 0, nil
}

// LevelByName returns the level named name, read without converting any other
// level, and whether one exists. A level that exists but cannot be read back
// as a HieraLevel (for example one that is not a mapping) returns (nil, true,
// nil): it is present, and it equals no level a caller could compare it to. An
// error is returned only when yamlText is not a parseable YAML mapping
// document.
func LevelByName(yamlText, name string) (*hostv1.HieraLevel, bool, error) {
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return nil, false, err
	}
	if doc == nil {
		return nil, false, nil
	}
	seq := mapValue(rootMapping(doc), "hierarchy")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil, false, nil
	}
	i := levelIndexByName(seq, name)
	if i < 0 {
		return nil, false, nil
	}
	lvl, err := levelFromNode(seq.Content[i])
	if err != nil {
		return nil, true, nil
	}
	return lvl, true, nil
}

// PutLevel adds or replaces one hierarchy level by editing the YAML node
// tree directly, so every comment in the input survives into the output.
// insert=true splices level in at index (clamped to the sequence's
// bounds); insert=false replaces the level whose name matches level.Name
// in place and ignores index, returning an error wrapping ErrHieraInvalid
// when no level carries that name. Empty or whitespace-only input text
// starts from EmptyHierarchy(). Text that carries only comments or
// empty-document markers keeps its comments above the emitted hierarchy;
// content-free text that cannot be classified that way is refused with an error
// wrapping ErrHieraInvalid (DQ-6-R, WR-01). Every failure returns an error and
// an empty string, never a partially-edited document.
func PutLevel(yamlText string, level *hostv1.HieraLevel, index int32, insert bool) (string, error) {
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return "", err
	}
	preamble := ""
	if doc == nil {
		if strings.TrimSpace(yamlText) != "" {
			if preamble, err = contentFreePreamble(yamlText); err != nil {
				return "", err
			}
		}
		doc, err = decodeDoc(EmptyHierarchy())
		if err != nil {
			return "", err
		}
	}
	m := rootMapping(doc)
	seq, err := hierarchySeq(m)
	if err != nil {
		return "", err
	}
	newNode := levelToNode(level)

	if insert {
		idx := int(index)
		if idx < 0 {
			idx = 0
		}
		if idx > len(seq.Content) {
			idx = len(seq.Content)
		}
		seq.Content = append(seq.Content, nil)
		copy(seq.Content[idx+1:], seq.Content[idx:])
		seq.Content[idx] = newNode
	} else {
		i := levelIndexByName(seq, level.GetName())
		if i == -1 {
			return "", fmt.Errorf("%w: no hierarchy level named %q", ErrHieraInvalid, level.GetName())
		}
		seq.Content[i] = newNode
	}

	out, err := encodeDoc(doc)
	if err != nil {
		return "", err
	}
	return preamble + out, nil
}

// RemoveLevel drops the named level from yamlText's hierarchy sequence
// and closes the gap. An unknown name is an error wrapping
// ErrHieraInvalid, leaving the document unmodified.
func RemoveLevel(yamlText string, name string) (string, error) {
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return "", err
	}
	if doc == nil {
		return "", fmt.Errorf("%w: no hierarchy level named %q", ErrHieraInvalid, name)
	}
	m := rootMapping(doc)
	seq := mapValue(m, "hierarchy")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return "", fmt.Errorf("%w: no hierarchy level named %q", ErrHieraInvalid, name)
	}
	i := levelIndexByName(seq, name)
	if i == -1 {
		return "", fmt.Errorf("%w: no hierarchy level named %q", ErrHieraInvalid, name)
	}
	seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
	return encodeDoc(doc)
}

// ReorderLevels applies names as the new hierarchy level order. names
// must be an exact permutation of the current level names — same length,
// same multiset — or ReorderLevels returns an error wrapping
// ErrHieraInvalid naming the discrepancy and leaves the input text
// unmodified (the returned string is always empty on error, and the
// document is never partially edited).
func ReorderLevels(yamlText string, names []string) (string, error) {
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return "", err
	}
	if doc == nil {
		return "", fmt.Errorf("%w: hierarchy has no levels to reorder", ErrHieraInvalid)
	}
	m := rootMapping(doc)
	seq := mapValue(m, "hierarchy")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return "", fmt.Errorf("%w: hierarchy has no levels to reorder", ErrHieraInvalid)
	}

	current := make([]string, len(seq.Content))
	byName := make(map[string]*yaml.Node, len(seq.Content))
	for i, c := range seq.Content {
		n := ""
		if nv := mapValue(c, "name"); nv != nil {
			n = nv.Value
		}
		current[i] = n
		byName[n] = c
	}

	if err := validatePermutation(current, names); err != nil {
		return "", err
	}

	newContent := make([]*yaml.Node, len(names))
	for i, n := range names {
		newContent[i] = byName[n]
	}
	seq.Content = newContent
	return encodeDoc(doc)
}

// validatePermutation returns an error naming the missing, extra or
// duplicated name when want is not an exact permutation of current.
func validatePermutation(current, want []string) error {
	if len(current) != len(want) {
		return fmt.Errorf("%w: reorder names %d levels but the hierarchy has %d", ErrHieraInvalid, len(want), len(current))
	}
	wantCount := make(map[string]int, len(want))
	for _, n := range want {
		wantCount[n]++
		if wantCount[n] > 1 {
			return fmt.Errorf("%w: reorder names %q more than once", ErrHieraInvalid, n)
		}
	}
	curCount := make(map[string]int, len(current))
	for _, n := range current {
		curCount[n]++
	}
	for n := range wantCount {
		if curCount[n] == 0 {
			return fmt.Errorf("%w: reorder names unknown level %q", ErrHieraInvalid, n)
		}
	}
	for n := range curCount {
		if wantCount[n] == 0 {
			return fmt.Errorf("%w: reorder is missing level %q", ErrHieraInvalid, n)
		}
	}
	return nil
}

// LintLevelPaths flags any %{environment}/%{::environment} interpolation
// in level's path, paths, glob, or the third (template) element of
// mapped_paths, naming the offending field. It never returns an error:
// per D-01, this is advisory-only and structurally cannot become a write
// refusal — the write it accompanies has already succeeded.
func LintLevelPaths(level *hostv1.HieraLevel) []*hostv1.LintWarning {
	var warnings []*hostv1.LintWarning
	if level == nil {
		return warnings
	}
	flag := func(field, value string) {
		if reEnvInterp.MatchString(value) {
			warnings = append(warnings, &hostv1.LintWarning{
				Code:  "hiera_environment_interpolation",
				Field: field,
				Message: fmt.Sprintf(
					"%s interpolates the environment name into a hierarchy path, coupling data layout to environment names — this is the %%{environment} anti-pattern Puppet's own best-practices guidance calls out",
					field,
				),
			})
		}
	}
	flag("path", level.GetPath())
	for i, p := range level.GetPaths() {
		flag(fmt.Sprintf("paths[%d]", i), p)
	}
	flag("glob", level.GetGlob())
	if mp := level.GetMappedPaths(); len(mp) == 3 {
		flag("mapped_paths[2]", mp[2])
	}
	return warnings
}

// --- data files (data/*.yaml) ---

// ParseDataFile turns a Hiera data file's YAML text into a
// *hostv1.HieraDataFile. Every top-level key other than the reserved
// lookup_options becomes a values entry, wrapped through nodeToJSON in
// the repo's single-field "v" convention (documents.go's Query and
// host/local/inventory.go's jsonScalar already rely on the same
// convention, so a value written here reads back identically through any
// other facet path). lookup_options is read separately into
// LookupOptions and excluded from Values. Empty or whitespace-only text
// returns a HieraDataFile with empty, non-nil Values and LookupOptions
// maps and a nil error — an unauthored data file is a legitimate state.
func ParseDataFile(yamlText string) (*hostv1.HieraDataFile, error) {
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return nil, err
	}
	df := &hostv1.HieraDataFile{
		Values:        map[string]*hostv1.Json{},
		LookupOptions: map[string]string{},
	}
	if doc == nil {
		return df, nil
	}
	m := rootMapping(doc)
	for i := 0; i+1 < len(m.Content); i += 2 {
		key := m.Content[i].Value
		val := m.Content[i+1]
		if key == "lookup_options" {
			if val.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				optKey := val.Content[j].Value
				df.LookupOptions[optKey] = lookupMergeValue(val.Content[j+1])
			}
			continue
		}
		j, err := nodeToJSON(val)
		if err != nil {
			return nil, err
		}
		df.Values[key] = j
	}
	return df, nil
}

// lookupMergeValue reads a lookup_options entry's merge behavior: the scalar
// value when merge is itself a scalar, or the strategy child when merge
// is a mapping (e.g. merge: {strategy: deep, ...}). It never builds or
// evaluates a deep merge — HIERA-03 is read-for-display only this
// milestone.
func lookupMergeValue(optNode *yaml.Node) string {
	merge := mapValue(optNode, "merge")
	if merge == nil {
		return ""
	}
	switch merge.Kind {
	case yaml.ScalarNode:
		return merge.Value
	case yaml.MappingNode:
		if sv := mapValue(merge, "strategy"); sv != nil {
			return sv.Value
		}
	}
	return ""
}

// nodeToJSON converts a data-file value node to *hostv1.Json, matching
// the repo's single-field wrapper convention: a scalar or sequence value
// is wrapped as {"v": value}; a mapping value is built directly as the
// Json's Struct, matching how host/local/inventory.go's jsonScalar builds
// an object-valued fact. This is the mirrored-helper convention Phase 3
// records after getting it wrong once — a value written through this
// path and a value read through any other facet path must be
// indistinguishable.
func nodeToJSON(n *yaml.Node) (*hostv1.Json, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHieraParse, err)
	}
	if m, ok := v.(map[string]any); ok {
		s, err := structpb.NewStruct(m)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrHieraInvalid, err)
		}
		return &hostv1.Json{Value: s}, nil
	}
	s, err := structpb.NewStruct(map[string]any{"v": v})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHieraInvalid, err)
	}
	return &hostv1.Json{Value: s}, nil
}

// jsonToNode is nodeToJSON's inverse: it unwraps the single-field "v"
// convention back into a YAML node ready to be spliced into a data file's
// node tree, quoting any resulting scalar that needsQuote requires.
func jsonToNode(j *hostv1.Json) (*yaml.Node, error) {
	if j == nil || j.Value == nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "~"}, nil
	}
	m := j.Value.AsMap()
	if v, ok := m["v"]; ok && len(m) == 1 {
		return valueToNode(v)
	}
	return valueToNode(m)
}

// valueToNode converts a decoded JSON value (as produced by
// structpb.Struct.AsMap/AsInterface: nil, bool, float64, string,
// []interface{}, map[string]interface{}) into a YAML node. Map keys are
// sorted for deterministic output, since a Go map carries no order of its
// own — this only affects a brand-new value being written, never an
// existing document's untouched keys, which are never round-tripped
// through this function.
func valueToNode(v any) (*yaml.Node, error) {
	switch t := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "~"}, nil
	case bool:
		val := "false"
		if t {
			val = "true"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: val}, nil
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: formatNumber(t)}, nil
	case string:
		return strNode(t), nil
	case []any:
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range t {
			n, err := valueToNode(item)
			if err != nil {
				return nil, err
			}
			seq.Content = append(seq.Content, n)
		}
		return seq, nil
	case map[string]any:
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			n, err := valueToNode(t[k])
			if err != nil {
				return nil, err
			}
			m.Content = append(m.Content, scalarNode(k, false), n)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("%w: unsupported JSON value type %T", ErrHieraInvalid, v)
	}
}

// formatNumber renders f as a plain integer when it carries no fractional
// part (JSON/structpb numbers are always float64, and a Hiera data value
// like "port: 8080" should not come back out as "8080.0"), or as a
// shortest-round-trip float otherwise.
func formatNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// PutDataKey edits one named key of a data file's YAML node tree, leaving
// every other key, its value, its comments and its relative order
// untouched. An existing key's value is replaced in place; a new key is
// appended at the end rather than sorted into place — a data file's key
// order is the operator's. The reserved key lookup_options is refused
// with an error wrapping ErrHieraInvalid before anything is decoded, so a
// refused write cannot leave a half-decoded document behind:
// lookup_options is readable for display and not writable this
// milestone (HIERA-03). Empty or whitespace-only input text starts from
// a fresh empty mapping document. Text that carries only comments or
// empty-document markers (a placeholder "# do not edit by hand" file, a bare
// "---", a null root) keeps its comments above the written key; content-free
// text that cannot be classified that way is refused with an error wrapping
// ErrHieraInvalid and an empty return, never rewritten (DQ-6-R, WR-01).
func PutDataKey(yamlText string, key string, value *hostv1.Json) (string, error) {
	if key == "lookup_options" {
		return "", fmt.Errorf("%w: lookup_options is read-only this milestone", ErrHieraInvalid)
	}
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return "", err
	}
	preamble := ""
	if doc == nil {
		if strings.TrimSpace(yamlText) != "" {
			if preamble, err = contentFreePreamble(yamlText); err != nil {
				return "", err
			}
		}
		doc = emptyMappingDoc()
	}
	m := rootMapping(doc)
	valNode, err := jsonToNode(value)
	if err != nil {
		return "", err
	}
	setMapValue(m, key, valNode)
	out, err := encodeDoc(doc)
	if err != nil {
		return "", err
	}
	return preamble + out, nil
}

// DataKeyExists reports whether yamlText's top-level mapping holds a key named
// exactly key. It walks the node tree and never converts a value, so a data file
// carrying a value ParseDataFile cannot represent (an integer-keyed map, a YAML
// timestamp) can still be asked about one key. It matches on the raw node key,
// the same comparison PutDataKey makes when it decides between replacing a value
// and appending a key, so "does it exist" and "would PutDataKey replace it"
// always agree. The reserved key lookup_options reports false: it is never a
// writable data key, and PutDataKey refuses it on its own terms. Empty or
// whitespace-only text has no key. An error is returned only when yamlText is
// not a parseable YAML mapping document.
func DataKeyExists(yamlText, key string) (bool, error) {
	if key == "lookup_options" {
		return false, nil
	}
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return false, err
	}
	if doc == nil {
		return false, nil
	}
	return mapValue(rootMapping(doc), key) != nil, nil
}

// DataKeyValue returns the value stored under key, converted the way
// ParseDataFile converts it but for that one key only, and whether the key
// exists. A key that exists but whose value cannot be represented as JSON
// returns (nil, true, nil): it is present, and it equals no value a caller could
// compare it to. lookup_options reports absent, as in DataKeyExists. An error is
// returned only when yamlText is not a parseable YAML mapping document.
func DataKeyValue(yamlText, key string) (*hostv1.Json, bool, error) {
	if key == "lookup_options" {
		return nil, false, nil
	}
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return nil, false, err
	}
	if doc == nil {
		return nil, false, nil
	}
	n := mapValue(rootMapping(doc), key)
	if n == nil {
		return nil, false, nil
	}
	j, err := nodeToJSON(n)
	if err != nil {
		return nil, true, nil
	}
	return j, true, nil
}

// RemoveDataKey drops the named key from a data file's YAML node tree,
// including its attached comments, and leaves everything else untouched.
// An unknown key is an error wrapping ErrHieraInvalid.
func RemoveDataKey(yamlText string, key string) (string, error) {
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return "", err
	}
	if doc == nil {
		return "", fmt.Errorf("%w: no key named %q", ErrHieraInvalid, key)
	}
	m := rootMapping(doc)
	if !deleteMapKey(m, key) {
		return "", fmt.Errorf("%w: no key named %q", ErrHieraInvalid, key)
	}
	return encodeDoc(doc)
}

// ValidateDataPath rejects a data-file relative path that is empty, that
// starts with '/', that contains a ".." segment, that contains a
// backslash, or that contains a NUL or newline byte. It runs today
// against an in-memory map where traversal cannot escape anything, so it
// protects nothing at this instant — it exists because a later
// real-backend implementation, filesystem- or database-backed, will not
// have that accidental immunity, and because the composite
// <env>/<relative-path> Documents doc id (06-01's contract_draft) only
// stays unambiguous while the path side is well-formed. A validation
// rule added after data exists is a migration, not a rule.
func ValidateDataPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: data path must not be empty", ErrHieraInvalid)
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: data path must not be absolute", ErrHieraInvalid)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return fmt.Errorf("%w: data path must not contain a %q segment", ErrHieraInvalid, "..")
		}
	}
	if strings.ContainsRune(path, '\\') {
		return fmt.Errorf("%w: data path must not contain a backslash", ErrHieraInvalid)
	}
	if strings.ContainsAny(path, "\x00\n") {
		return fmt.Errorf("%w: data path must not contain a NUL or newline byte", ErrHieraInvalid)
	}
	return nil
}
