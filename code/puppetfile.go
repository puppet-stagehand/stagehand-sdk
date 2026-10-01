// Package code holds the pure parse/render logic for the control-repo file
// formats the Code facet models: the Puppetfile Ruby DSL (this file) and
// environment.conf (envconf.go). It operates on the generated hostv1
// message types directly so that PF-05's round-trip guarantee is a
// property of the actual wire model rather than of a conversion layer
// nobody tests. It has no dependency on any gRPC package or on
// host/local, so it is unit-testable in isolation exactly as approval/ is.
package code

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// ErrPuppetfileParse is wrapped by every error ParsePuppetfile returns when
// the input text cannot be read as valid Puppetfile DSL. host/local/
// code_puppetfile.go (Plan 06-06) maps it to codes.InvalidArgument via
// errors.Is, never by string matching.
var ErrPuppetfileParse = errors.New("puppetfile: parse error")

// ErrPuppetfileInvalid is wrapped by every error ValidateModule and
// RenderPuppetfile return when a *hostv1.PuppetfileModule cannot be
// faithfully rendered back to DSL text.
var ErrPuppetfileInvalid = errors.New("puppetfile: invalid module")

var (
	// reModuledir matches a `moduledir 'path'` or `moduledir "path"` line.
	reModuledir = regexp.MustCompile(`^moduledir\s+['"]([^'"]+)['"]\s*$`)
	// reMod matches a `mod 'name'` line, capturing the name and everything
	// after the optional trailing `, ...` remainder.
	reMod = regexp.MustCompile(`^mod\s+['"]([^'"]+)['"]\s*(?:,\s*(.*))?$`)
	// reQuoted matches a remainder that is nothing but a single- or
	// double-quoted scalar — the Forge pinned-version form.
	reQuoted = regexp.MustCompile(`^(?:'[^']*'|"[^"]*")$`)
	// reGitAttr extracts a recognised `:key => value` pair from a Git
	// module's remainder. value is either a bare Ruby symbol (used only by
	// the control-branch sentinel), or a single- or double-quoted scalar.
	reGitAttr = regexp.MustCompile(`:(git|ref|tag|branch|commit|default_branch)\s*=>\s*(:[A-Za-z_][A-Za-z0-9_]*|'[^']*'|"[^"]*")`)
	// reAttrKey matches any `:key =>` pair regardless of whether key is
	// recognised, so an unmodelled attribute can be named in an error
	// rather than silently skipped.
	reAttrKey = regexp.MustCompile(`:([A-Za-z_][A-Za-z0-9_]*)\s*=>`)
	// reHasGit reports whether a mod remainder carries a :git attribute,
	// the signal that distinguishes a Git-sourced module from a Forge one.
	reHasGit = regexp.MustCompile(`:git\s*=>`)
)

// knownGitAttrKeys is the six DSL attribute keys this package models on a
// Git-sourced module. control_branch is deliberately absent — the real
// grammar spells it as the value :control_branch of the branch key, not as
// a key of its own (06-RESEARCH.md Pitfall 2).
var knownGitAttrKeys = map[string]bool{
	"git": true, "ref": true, "tag": true, "branch": true, "commit": true, "default_branch": true,
}

// logicalLine is one Ruby method-call statement: comment-stripped and
// continuation-joined, carrying the 1-based physical line number of its
// first line for error messages.
type logicalLine struct {
	text string
	line int
}

// stripComment removes everything from the first unquoted '#' onward. A
// naive strip would corrupt a git URL carrying a fragment.
func stripComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '#':
			return line[:i]
		}
	}
	return line
}

// logicalLines splits text on '\n', strips comments, trims blank results,
// and joins any line whose trimmed form ends with a comma onto the
// following line with a single space, because Ruby method-call syntax
// spans lines that way.
func logicalLines(text string) []logicalLine {
	raw := strings.Split(text, "\n")
	var out []logicalLine
	var cur strings.Builder
	curLine := 0
	for i, r := range raw {
		lineNo := i + 1
		t := strings.TrimSpace(stripComment(r))
		if t == "" {
			continue
		}
		if cur.Len() == 0 {
			curLine = lineNo
			cur.WriteString(t)
		} else {
			cur.WriteString(" ")
			cur.WriteString(t)
		}
		if strings.HasSuffix(cur.String(), ",") {
			continue
		}
		out = append(out, logicalLine{text: cur.String(), line: curLine})
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, logicalLine{text: cur.String(), line: curLine})
	}
	return out
}

// parseQuotedScalar strips one layer of matching single or double quotes
// from s, reporting whether s was fully quoted. It is used instead of a
// regex capture group because Go's regexp reports an unmatched group and a
// matched-but-empty group identically (both as ""), which would make a
// quoted empty string indistinguishable from "not quoted at all".
func parseQuotedScalar(s string) (string, bool) {
	if len(s) >= 2 {
		if s[0] == '\'' && s[len(s)-1] == '\'' {
			return s[1 : len(s)-1], true
		}
		if s[0] == '"' && s[len(s)-1] == '"' {
			return s[1 : len(s)-1], true
		}
	}
	return "", false
}

// attrValue unwraps a reGitAttr-captured value: a quoted scalar is
// unquoted, a bare Ruby symbol (e.g. :control_branch) is returned as-is so
// the caller can recognise it by its leading colon.
func attrValue(raw string) string {
	if v, ok := parseQuotedScalar(raw); ok {
		return v
	}
	return raw
}

// ParsePuppetfile turns Puppetfile DSL text into a *hostv1.Puppetfile
// carrying moduledir and an ordered modules slice. An empty or
// comment-only text parses to zero modules, an empty Moduledir and a nil
// error.
func ParsePuppetfile(text string) (*hostv1.Puppetfile, error) {
	pf := &hostv1.Puppetfile{}
	seenModuledir := false
	for _, ll := range logicalLines(text) {
		if m := reModuledir.FindStringSubmatch(ll.text); m != nil {
			if seenModuledir {
				return nil, fmt.Errorf("%w: line %d: duplicate moduledir", ErrPuppetfileParse, ll.line)
			}
			seenModuledir = true
			pf.Moduledir = m[1]
			continue
		}
		if m := reMod.FindStringSubmatch(ll.text); m != nil {
			name := m[1]
			remainder := strings.TrimSpace(m[2])
			mod, err := parseModuleRemainder(name, remainder, ll.line)
			if err != nil {
				return nil, err
			}
			pf.Modules = append(pf.Modules, mod)
			continue
		}
		return nil, fmt.Errorf("%w: line %d: unrecognized line %q", ErrPuppetfileParse, ll.line, ll.text)
	}
	return pf, nil
}

// parseModuleRemainder builds a *hostv1.PuppetfileModule from a mod line's
// name and the (possibly empty) text after its first comma. A remainder
// carrying a :git attribute is Git-sourced; otherwise it is Forge-sourced.
func parseModuleRemainder(name, remainder string, lineNo int) (*hostv1.PuppetfileModule, error) {
	if reHasGit.MatchString(remainder) {
		return parseGitModule(name, remainder, lineNo)
	}
	mod := &hostv1.PuppetfileModule{Name: name}
	switch {
	case remainder == "":
		mod.Source = &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}
	case remainder == ":latest":
		mod.Source = &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Latest: true}}
	case reQuoted.MatchString(remainder):
		v, _ := parseQuotedScalar(remainder)
		mod.Source = &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: v}}
	default:
		if keys := reAttrKey.FindAllStringSubmatch(remainder, -1); len(keys) > 0 {
			return nil, fmt.Errorf("%w: line %d: unrecognized attribute :%s", ErrPuppetfileParse, lineNo, keys[0][1])
		}
		return nil, fmt.Errorf("%w: line %d: unrecognized mod attributes %q", ErrPuppetfileParse, lineNo, remainder)
	}
	return mod, nil
}

// gitAttr is one recognised `:key => value` pair extracted from a Git
// module's remainder, with value still in its raw (possibly quoted) form.
type gitAttr struct {
	key string
	raw string
}

// parseGitModule parses a Git-sourced mod block's remainder into a
// *hostv1.GitSource. It enforces D-02 (mutual exclusivity across
// :ref/:tag/:branch/:commit) at runtime — the oneof ref_kind is D-02's
// structural half, making two selectors impossible to construct in Go, so
// the only place two can arrive is from text. :default_branch (D-03) is
// outside the exclusivity group entirely and may accompany any ref arm or
// none.
func parseGitModule(name, remainder string, lineNo int) (*hostv1.PuppetfileModule, error) {
	for _, m := range reAttrKey.FindAllStringSubmatch(remainder, -1) {
		if !knownGitAttrKeys[m[1]] {
			return nil, fmt.Errorf("%w: line %d: unrecognized attribute :%s", ErrPuppetfileParse, lineNo, m[1])
		}
	}

	var attrs []gitAttr
	for _, m := range reGitAttr.FindAllStringSubmatch(remainder, -1) {
		attrs = append(attrs, gitAttr{key: m[1], raw: m[2]})
	}

	gs := &hostv1.GitSource{}
	var refKeys []string
	for _, a := range attrs {
		switch a.key {
		case "git":
			gs.Url = attrValue(a.raw)
		case "default_branch":
			gs.DefaultBranch = attrValue(a.raw)
		case "ref", "tag", "branch", "commit":
			refKeys = append(refKeys, a.key)
		}
	}

	if len(refKeys) > 1 {
		return nil, fmt.Errorf("%w: line %d: conflicting ref selectors :%s and :%s", ErrPuppetfileParse, lineNo, refKeys[0], refKeys[1])
	}
	if len(refKeys) == 1 {
		key := refKeys[0]
		var raw string
		for _, a := range attrs {
			if a.key == key {
				raw = a.raw
				break
			}
		}
		switch key {
		case "ref":
			gs.RefKind = &hostv1.GitSource_Ref{Ref: attrValue(raw)}
		case "tag":
			gs.RefKind = &hostv1.GitSource_Tag{Tag: attrValue(raw)}
		case "commit":
			gs.RefKind = &hostv1.GitSource_Commit{Commit: attrValue(raw)}
		case "branch":
			if raw == ":control_branch" {
				gs.RefKind = &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}}
			} else {
				gs.RefKind = &hostv1.GitSource_Branch{Branch: attrValue(raw)}
			}
		}
	}

	return &hostv1.PuppetfileModule{Name: name, Source: &hostv1.PuppetfileModule_Git{Git: gs}}, nil
}

// RenderPuppetfile emits canonical DSL text for p, matching
// <render_contract> exactly: single quotes throughout, one line per Forge
// module, a mod line plus two-space-indented attribute lines per Git
// module, and a trailing newline. An empty model renders as the empty
// string, with no newline.
func RenderPuppetfile(p *hostv1.Puppetfile) (string, error) {
	if p == nil {
		return "", nil
	}
	var b strings.Builder
	if p.Moduledir != "" {
		fmt.Fprintf(&b, "moduledir '%s'\n", p.Moduledir)
		if len(p.Modules) > 0 {
			b.WriteString("\n")
		}
	}
	for _, m := range p.Modules {
		if err := ValidateModule(m); err != nil {
			return "", err
		}
		switch src := m.Source.(type) {
		case *hostv1.PuppetfileModule_Forge:
			b.WriteString(renderForge(m.Name, src.Forge))
		case *hostv1.PuppetfileModule_Git:
			b.WriteString(renderGit(m.Name, src.Git))
		default:
			return "", fmt.Errorf("%w: module %q has no source", ErrPuppetfileInvalid, m.Name)
		}
	}
	return b.String(), nil
}

// renderForge renders a Forge module's single line: bare, pinned, or
// :latest, per <render_contract> rule 3.
func renderForge(name string, f *hostv1.ForgeSource) string {
	switch {
	case f.GetLatest():
		return fmt.Sprintf("mod '%s', :latest\n", name)
	case f.GetVersion() != "":
		return fmt.Sprintf("mod '%s', '%s'\n", name, f.GetVersion())
	default:
		return fmt.Sprintf("mod '%s'\n", name)
	}
}

// renderGit renders a Git module's block per <render_contract> rules 4 and
// 5: a `mod 'name',` opener, then one two-space-indented attribute line
// per set field, in the fixed order :git, ref_kind arm, :default_branch —
// every line but the last ends with a comma. The control-branch arm is the
// one attribute value emitted unquoted, as a bare Ruby symbol, because
// that is the only valid DSL spelling (06-RESEARCH.md Pitfall 2);
// emitting it as its own key would produce text this package's own parser
// rejects.
func renderGit(name string, g *hostv1.GitSource) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("  :git => '%s'", g.GetUrl()))
	switch rk := g.GetRefKind().(type) {
	case *hostv1.GitSource_Ref:
		lines = append(lines, fmt.Sprintf("  :ref => '%s'", rk.Ref))
	case *hostv1.GitSource_Tag:
		lines = append(lines, fmt.Sprintf("  :tag => '%s'", rk.Tag))
	case *hostv1.GitSource_Branch:
		lines = append(lines, fmt.Sprintf("  :branch => '%s'", rk.Branch))
	case *hostv1.GitSource_Commit:
		lines = append(lines, fmt.Sprintf("  :commit => '%s'", rk.Commit))
	case *hostv1.GitSource_ControlBranch:
		lines = append(lines, "  :branch => :control_branch")
	}
	if g.GetDefaultBranch() != "" {
		lines = append(lines, fmt.Sprintf("  :default_branch => '%s'", g.GetDefaultBranch()))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "mod '%s',\n", name)
	for i, line := range lines {
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// reForgeSlug matches the Forge slug grammar: an owner segment of
// alphanumerics, a single '-' or '/', then a module segment starting with
// a lowercase letter followed by lowercase alphanumerics and underscores.
var reForgeSlug = regexp.MustCompile(`^[A-Za-z0-9]+[-/][a-z][a-z0-9_]*$`)

// forgeSlugOK reports whether name is a valid Forge module slug
// (puppetlabs/apache or puppetlabs-apache), per PF-02/PF-03's boundary
// case.
func forgeSlugOK(name string) bool {
	return reForgeSlug.MatchString(name)
}

// reGitTransport matches the five accepted URL-scheme transports.
var reGitTransport = regexp.MustCompile(`^(?:https?|git|ssh|file)://`)

// reGitSCP matches the SCP-style user@host:path form.
var reGitSCP = regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+:.+$`)

// gitURLOK reports whether url is safe and well-formed for this facet's
// model: non-empty, free of any whitespace byte (which would forge an
// additional Puppetfile attribute line), free of a ProxyCommand option or
// an ext:: command-executing transport prefix (the two documented ways a
// git remote can be made to execute a command — T-06-04), and shaped as
// one of the five accepted URL transports or the SCP-style form.
//
// This rule governs what the Puppetfile MODEL may store, and it is
// deliberately wider than what the host may dial: a control repo legitimately
// references git:// and http:// module sources, so file://, git:// and http://
// are accepted here. It must not be reused as the import clone allowlist. The
// clone allowlist is https and ssh only, refuses any credential in the URL,
// and lives in host/local/git_client.go (validateGitURL); the host's git
// client also sets GIT_ALLOW_PROTOCOL as a second line of defence.
func gitURLOK(url string) bool {
	if url == "" {
		return false
	}
	if strings.ContainsAny(url, " \t\r\n") {
		return false
	}
	if strings.Contains(url, "ProxyCommand") {
		return false
	}
	if strings.Contains(url, "ext::") {
		return false
	}
	if reGitTransport.MatchString(url) {
		return true
	}
	return reGitSCP.MatchString(url)
}

// ValidateModule rejects a Forge module with both version and latest set,
// a Forge module whose name is not a valid slug, a Git module with an
// empty url, a Git module whose url carries whitespace or a newline, and a
// Git module whose url uses a command-executing transport or carries a
// proxy-command option.
func ValidateModule(m *hostv1.PuppetfileModule) error {
	name := m.GetName()
	if name == "" {
		return fmt.Errorf("%w: module name must not be empty", ErrPuppetfileInvalid)
	}
	switch {
	case m.GetForge() != nil:
		f := m.GetForge()
		if f.GetVersion() != "" && f.GetLatest() {
			return fmt.Errorf("%w: module %q sets both version and latest", ErrPuppetfileInvalid, name)
		}
		if !forgeSlugOK(name) {
			return fmt.Errorf("%w: module %q is not a valid Forge slug (expected ns/name or ns-name)", ErrPuppetfileInvalid, name)
		}
	case m.GetGit() != nil:
		url := m.GetGit().GetUrl()
		if !gitURLOK(url) {
			return fmt.Errorf("%w: module %q has an invalid git url %q", ErrPuppetfileInvalid, name, url)
		}
	default:
		return fmt.Errorf("%w: module %q has no source", ErrPuppetfileInvalid, name)
	}
	return nil
}
