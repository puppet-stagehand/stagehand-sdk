package code

// The lenient Puppetfile entry point. Like import.go it is part of the pure
// format layer: hostv1, protobuf and the standard library only, never
// approval, host or host/local.

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// ParsePuppetfileLenient turns Puppetfile text into a model plus findings
// instead of failing on the first statement the write-side model cannot
// represent (IMP-02). It returns no error: a Puppetfile is never unparseable
// as a whole, because every statement can be skipped, so every finding is a
// warning (D-10).
//
// It is one grammar with ParsePuppetfile, not a second one: it iterates the
// same logicalLines, matches the same regexes and calls the same per-statement
// parse functions, differing only in that each failure becomes a finding and
// the Ruby-1.9 key form is accepted (DQ-5). Every module it returns passes
// ValidateModule and survives RenderPuppetfile followed by strict
// ParsePuppetfile unchanged, which is what lets ApplyImport's second pass be
// infallible (RESEARCH Pitfall 2, T-10-15).
//
// Findings carry the parser's line numbers and an excerpt but no branch or
// file; stampFindings sets those.
func ParsePuppetfileLenient(text string, lim ImportLimits) (*hostv1.Puppetfile, []*hostv1.ImportFinding) {
	lim = lim.withDefaults()
	text = strings.TrimPrefix(text, "\xEF\xBB\xBF")
	pf := &hostv1.Puppetfile{}
	fl := newFindingList(lim)
	warn := func(kind string, line int, excerpt, msg string) {
		fl.add(newFinding(kind, hostv1.ImportFinding_WARNING, line, excerpt, msg, lim))
	}

	seenModuledir := false
	seenNames := map[string]bool{}
	for _, ll := range logicalLines(text) {
		if m := reModuledir.FindStringSubmatch(ll.text); m != nil {
			if !utf8.ValidString(m[1]) {
				warn(FindingPuppetfileUnsupportedRuby, ll.line, ll.text, "the moduledir is not valid UTF-8 and cannot be stored; it was skipped")
				continue
			}
			if seenModuledir {
				warn(FindingPuppetfileDuplicateModuledir, ll.line, ll.text, "moduledir was already set; the last value is kept, matching Ruby evaluation order")
			}
			seenModuledir = true
			pf.Moduledir = m[1]
			continue
		}
		if m := reMod.FindStringSubmatch(ll.text); m != nil {
			name := m[1]
			remainder := strings.TrimSpace(m[2])
			if kind, msg := lenientRemainderProblem(remainder); kind != "" {
				warn(kind, ll.line, ll.text, msg)
				continue
			}
			mod, err := parseModuleRemainder(name, remainder, ll.line, true)
			if err != nil {
				// Unreachable after lenientRemainderProblem; kept so a future
				// grammar change degrades to a finding rather than a panic.
				warn(FindingPuppetfileUnsupportedRuby, ll.line, ll.text, "this mod statement uses a form the module model cannot represent; it was skipped")
				continue
			}
			if err := ValidateModule(mod); err != nil {
				warn(FindingPuppetfileInvalidModule, ll.line, ll.text, fmt.Sprintf("the module would not render back to a Puppetfile (%v); it was skipped", err))
				continue
			}
			if !utf8.ValidString(ll.text) || !moduleSurvivesRender(mod) {
				warn(FindingPuppetfileInvalidModule, ll.line, ll.text, "the module does not survive a render and strict re-parse unchanged (an embedded quote or invalid UTF-8); it was skipped")
				continue
			}
			if seenNames[mod.GetName()] {
				warn(FindingPuppetfileDuplicateModule, ll.line, ll.text, "module names are unique in the model; the first occurrence is kept and this one was skipped")
				continue
			}
			seenNames[mod.GetName()] = true
			pf.Modules = append(pf.Modules, mod)
			continue
		}
		if isForgeDirective(ll.text) {
			warn(FindingPuppetfileForgeDirective, ll.line, ll.text, "the model has no forge-URL field; the directive was skipped and the rest of the file imported")
			continue
		}
		if isHashFormMod(ll.text) {
			warn(FindingPuppetfileUnmodelledAttribute, ll.line, ll.text, "a hash-form mod statement carries attributes the module model cannot represent; the whole statement was skipped")
			continue
		}
		warn(FindingPuppetfileUnsupportedRuby, ll.line, ll.text, "this line is Ruby the Puppetfile model does not represent; it was skipped")
	}
	return pf, fl.items
}

// lenientRemainderProblem inspects a mod statement's remainder, before the
// shared parse functions run, and names why the statement cannot be imported
// whole. It returns an empty kind when the statement is representable. Every
// non-empty answer means the WHOLE statement is skipped: importing a module
// minus an attribute it declared would silently change where or what it
// deploys (D-09, T-10-18).
func lenientRemainderProblem(remainder string) (kind, msg string) {
	norm := normalizeRuby19Keys(remainder)

	if !reHasGit.MatchString(norm) {
		if norm == "" || norm == ":latest" || reQuoted.MatchString(norm) {
			return "", ""
		}
		if keys := reAttrKey.FindAllStringSubmatch(norm, -1); len(keys) > 0 {
			return FindingPuppetfileUnmodelledAttribute, unmodelledMsg(keys[0][1])
		}
		return FindingPuppetfileUnsupportedRuby, "the mod statement's arguments are not a version, :latest or a git source; it was skipped"
	}

	for _, m := range reAttrKey.FindAllStringSubmatch(norm, -1) {
		if !knownGitAttrKeys[m[1]] {
			return FindingPuppetfileUnmodelledAttribute, unmodelledMsg(m[1])
		}
	}
	attrs := reGitAttr.FindAllStringSubmatch(norm, -1)
	var refKeys []string
	for _, a := range attrs {
		switch a[1] {
		case "ref", "tag", "branch", "commit":
			refKeys = append(refKeys, a[1])
		}
		// A bare symbol is only meaningful as the control-branch sentinel;
		// anywhere else it would import as a literal ":name" string.
		if strings.HasPrefix(a[2], ":") && !(a[1] == "branch" && a[2] == ":control_branch") {
			return FindingPuppetfileUnsupportedRuby, fmt.Sprintf("the :%s attribute has a Ruby symbol value the model cannot represent; the statement was skipped", a[1])
		}
	}
	if len(refKeys) > 1 {
		return FindingPuppetfileConflictingRef, fmt.Sprintf("a git module may pin only one of :ref, :tag, :branch, :commit; this one sets :%s and :%s, so the statement was skipped", refKeys[0], refKeys[1])
	}
	// Whatever the recognised attributes do not account for (an attribute
	// whose value is a variable or an expression, stray tokens) would be
	// silently ignored by the shared parser, so refuse the statement instead.
	// The computation lives in puppetfile.go as gitRemainderLeftover so strict
	// and lenient parsing share one rule and cannot drift.
	if gitRemainderLeftover(norm) != "" {
		return FindingPuppetfileUnsupportedRuby, "the mod statement has attribute values the model cannot represent (a variable or expression); it was skipped"
	}
	return "", ""
}

func unmodelledMsg(key string) string {
	return fmt.Sprintf("the :%s attribute is not in the module model; the whole mod statement was skipped rather than imported without it", key)
}

// isHashFormMod reports whether a line is `mod :key => value`: the hash-form
// Forge module, which the model has no representation for.
func isHashFormMod(line string) bool {
	rest, ok := strings.CutPrefix(line, "mod")
	if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return false
	}
	rest = strings.TrimSpace(rest)
	return strings.HasPrefix(rest, ":") && reAttrKey.MatchString(normalizeRuby19Keys(rest))
}

// isForgeDirective reports whether a logical line is the r10k `forge '...'`
// directive: the word forge followed by whitespace, a quote or a paren.
func isForgeDirective(line string) bool {
	rest, ok := strings.CutPrefix(line, "forge")
	if !ok || rest == "" {
		return false
	}
	switch rest[0] {
	case ' ', '\t', '\'', '"', '(':
		return true
	}
	return false
}

// moduleSurvivesRender reports whether mod, rendered alone and strict-parsed
// back, comes out equal. It is the general guard behind ValidateModule: a
// value the renderer cannot re-quote faithfully (an embedded quote character)
// would otherwise import as one thing and re-read as another.
func moduleSurvivesRender(mod *hostv1.PuppetfileModule) bool {
	txt, err := RenderPuppetfile(&hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{mod}})
	if err != nil {
		return false
	}
	back, err := ParsePuppetfile(txt)
	if err != nil || len(back.GetModules()) != 1 {
		return false
	}
	return proto.Equal(back.GetModules()[0], mod)
}
