package code

// This file is the lenient environment.conf entry point (IMP-04). Like the
// rest of the package it is pure: hostv1, strings and utf8 only.

import (
	"fmt"
	"strings"
	"unicode/utf8"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// ParseEnvConfLenient maps environment.conf text onto a settings record the
// way ParseEnvConf does, but turns each of the strict parser's failure points
// into a warning and carries on, so one unfamiliar line never costs the whole
// file (D-09). Every finding is a warning: environment.conf is never
// unparseable as a whole. Findings are unstamped.
//
// Dispositions, one warning each, the offending line skipped and the rest of
// the file kept:
//   - a key outside envConfKeys is an unrecognised-key warning;
//   - a line with no equals sign is a malformed-line warning. A "[main]"
//     section header has no equals sign, so it is classified here, once, and
//     the keys under it are still read;
//   - a boolean key with a value other than true or false is an
//     invalid-boolean warning and leaves that field unset;
//   - a value RenderEnvConf would refuse (it carries a bare carriage return,
//     or is not valid UTF-8) is a malformed-line warning and leaves the field
//     unset, so the result can never fail RenderEnvConf and ApplyImport's
//     second pass stays infallible (T-10-41).
//
// A leading BOM is stripped, which the strict parser cannot do. A result with
// every field unset is returned as nil: absent stays absent, and no settings
// key is ever written for it (Phase 6's presence semantics).
//
// The key list, the boolean parser and the safety rule are the strict
// parser's own helpers, so they stay one source of truth.
func ParseEnvConfLenient(text string, lim ImportLimits) (*hostv1.EnvironmentSettings, []*hostv1.ImportFinding) {
	lim = lim.withDefaults()
	text = strings.TrimPrefix(text, utf8BOM)
	fl := newFindingList(lim)
	warn := func(kind string, line int, excerpt, msg string) {
		fl.add(newFinding(kind, hostv1.ImportFinding_WARNING, line, excerpt, msg, lim))
	}

	s := &hostv1.EnvironmentSettings{}
	haveSetting := false
	for i, raw := range strings.Split(text, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			warn(FindingEnvConfMalformedLine, lineNo, line,
				"the line has no '=' (a section header such as [main] is one), so it was skipped")
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if !isRecognizedEnvConfKey(key) {
			warn(FindingEnvConfUnrecognizedKey, lineNo, line,
				fmt.Sprintf("%q is not an environment.conf setting the Code model carries, so the line was skipped", key))
			continue
		}
		switch key {
		case "disable_per_environment_manifest", "static_catalogs", "rich_data":
			b, ok := parseBool(value)
			if !ok {
				warn(FindingEnvConfInvalidBoolean, lineNo, line,
					fmt.Sprintf("%s must be true or false, got %q, so the setting was left unset", key, value))
				continue
			}
			switch key {
			case "disable_per_environment_manifest":
				s.DisablePerEnvironmentManifest = &b
			case "static_catalogs":
				s.StaticCatalogs = &b
			case "rich_data":
				s.RichData = &b
			}
			haveSetting = true
		default:
			if !valueIsSafe(value) || !utf8.ValidString(value) {
				warn(FindingEnvConfMalformedLine, lineNo, line,
					fmt.Sprintf("the value of %s carries a bare carriage return or is not valid UTF-8 and cannot be stored faithfully, so the setting was left unset", key))
				continue
			}
			v := value
			switch key {
			case "modulepath":
				s.Modulepath = &v
			case "manifest":
				s.Manifest = &v
			case "config_version":
				s.ConfigVersion = &v
			case "environment_timeout":
				s.EnvironmentTimeout = &v
			}
			haveSetting = true
		}
	}
	if !haveSetting {
		return nil, fl.items
	}
	return s, fl.items
}
