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
	"strings"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	yaml "go.yaml.in/yaml/v3"
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
// to report. A non-empty document whose root is not a mapping is a parse
// error, wrapping ErrHieraParse.
func decodeDoc(text string) (*yaml.Node, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHieraParse, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: expected a YAML mapping document", ErrHieraParse)
	}
	return &doc, nil
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
// already exists in m, preserving that key's own comments and position.
// When key does not exist, the pair is appended at the end of m.Content
// rather than sorted into place — a data file's or hierarchy's key/level
// order belongs to the operator or caller, not to this package.
func setMapValue(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
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

// PutLevel adds or replaces one hierarchy level by editing the YAML node
// tree directly, so every comment in the input survives into the output.
// insert=true splices level in at index (clamped to the sequence's
// bounds); insert=false replaces the level whose name matches level.Name
// in place and ignores index, returning an error wrapping ErrHieraInvalid
// when no level carries that name. Empty or whitespace-only input text
// starts from EmptyHierarchy(). Every failure returns an error and an
// empty string, never a partially-edited document.
func PutLevel(yamlText string, level *hostv1.HieraLevel, index int32, insert bool) (string, error) {
	doc, err := decodeDoc(yamlText)
	if err != nil {
		return "", err
	}
	if doc == nil {
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

	return encodeDoc(doc)
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
