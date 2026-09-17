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

// parseGitModule parses a Git-sourced mod block's remainder. Filled in
// fully by Plan 06-03 Task 2 (D-02/D-03 ref-selector handling); this task
// only detects the :git attribute and refuses the block so RenderPuppetfile's
// Forge path can be proven first.
func parseGitModule(name, remainder string, lineNo int) (*hostv1.PuppetfileModule, error) {
	return nil, fmt.Errorf("%w: line %d: git-sourced modules not yet supported", ErrPuppetfileParse, lineNo)
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

// renderGit is filled in by Plan 06-03 Task 2, per <render_contract>
// rules 4 and 5.
func renderGit(name string, g *hostv1.GitSource) string {
	return ""
}

// ValidateModule is fully implemented by Plan 06-03 Task 2; this task
// stubs the two checks RenderPuppetfile's Forge path needs: a non-empty
// name, and a ForgeSource that does not set both Version and Latest.
func ValidateModule(m *hostv1.PuppetfileModule) error {
	if m.GetName() == "" {
		return fmt.Errorf("%w: module name must not be empty", ErrPuppetfileInvalid)
	}
	if f := m.GetForge(); f != nil {
		if f.GetVersion() != "" && f.GetLatest() {
			return fmt.Errorf("%w: module %q sets both version and latest", ErrPuppetfileInvalid, m.GetName())
		}
	}
	return nil
}
