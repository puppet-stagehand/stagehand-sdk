package code

import (
	"errors"
	"fmt"
	"strings"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// ErrEnvConfParse is wrapped by every error ParseEnvConf returns when the
// input text cannot be read as a valid environment.conf record.
var ErrEnvConfParse = errors.New("envconf: parse error")

// ErrEnvConfInvalid is wrapped by every error RenderEnvConf returns when a
// *hostv1.EnvironmentSettings value cannot be faithfully rendered.
var ErrEnvConfInvalid = errors.New("envconf: invalid value")

// envConfKeys is the ordered list of the seven recognised environment.conf
// keys, in <render_contract> declaration order. Both ParseEnvConf's key
// lookup and RenderEnvConf's emission order read from this one slice, so
// the two can never disagree about which keys exist or in what order they
// appear.
var envConfKeys = []string{
	"modulepath",
	"manifest",
	"config_version",
	"environment_timeout",
	"disable_per_environment_manifest",
	"static_catalogs",
	"rich_data",
}

func isRecognizedEnvConfKey(key string) bool {
	for _, k := range envConfKeys {
		if k == key {
			return true
		}
	}
	return false
}

// parseBool matches "true"/"false" case-insensitively and rejects every
// other form (including the numeric/letter forms Go's strconv.ParseBool
// otherwise accepts) — environment.conf's booleans are exactly the two
// literal words.
func parseBool(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// boolStr renders b as environment.conf's lowercase boolean literal.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// valueIsSafe reports whether s can be emitted as an environment.conf
// value without forging an additional, unauthored setting line. A value
// carrying a carriage return or line feed would split into what a later
// reader parses as a new line (T-06-02).
func valueIsSafe(s string) bool {
	return !strings.ContainsAny(s, "\r\n")
}

// ParseEnvConf maps each recognised "key = value" line onto the matching
// *hostv1.EnvironmentSettings field and leaves every field a pack never
// wrote as nil. Blank text (or text containing only comments and blank
// lines) yields a settings record whose seven setting fields are all nil.
func ParseEnvConf(text string) (*hostv1.EnvironmentSettings, error) {
	s := &hostv1.EnvironmentSettings{}
	for i, raw := range strings.Split(text, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			return nil, fmt.Errorf("%w: line %d: missing '=': %q", ErrEnvConfParse, lineNo, line)
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if !isRecognizedEnvConfKey(key) {
			return nil, fmt.Errorf("%w: line %d: unrecognized key %q", ErrEnvConfParse, lineNo, key)
		}
		switch key {
		case "modulepath":
			v := value
			s.Modulepath = &v
		case "manifest":
			v := value
			s.Manifest = &v
		case "config_version":
			v := value
			s.ConfigVersion = &v
		case "environment_timeout":
			v := value
			s.EnvironmentTimeout = &v
		case "disable_per_environment_manifest":
			b, ok := parseBool(value)
			if !ok {
				return nil, fmt.Errorf("%w: line %d: %s must be true or false, got %q", ErrEnvConfParse, lineNo, key, value)
			}
			s.DisablePerEnvironmentManifest = &b
		case "static_catalogs":
			b, ok := parseBool(value)
			if !ok {
				return nil, fmt.Errorf("%w: line %d: %s must be true or false, got %q", ErrEnvConfParse, lineNo, key, value)
			}
			s.StaticCatalogs = &b
		case "rich_data":
			b, ok := parseBool(value)
			if !ok {
				return nil, fmt.Errorf("%w: line %d: %s must be true or false, got %q", ErrEnvConfParse, lineNo, key, value)
			}
			s.RichData = &b
		}
	}
	return s, nil
}

// RenderEnvConf walks envConfKeys in order and emits "key = value" for
// every field that is set, skipping every nil field entirely per D-06 — a
// setting a pack never wrote is never backfilled with Puppet's documented
// default. Returns an error wrapping ErrEnvConfInvalid, and emits nothing
// at all, when a string value carries a carriage return or line feed.
func RenderEnvConf(s *hostv1.EnvironmentSettings) (string, error) {
	if s == nil {
		return "", nil
	}
	var lines []string
	for _, key := range envConfKeys {
		var value string
		switch key {
		case "modulepath":
			if s.Modulepath == nil {
				continue
			}
			value = *s.Modulepath
		case "manifest":
			if s.Manifest == nil {
				continue
			}
			value = *s.Manifest
		case "config_version":
			if s.ConfigVersion == nil {
				continue
			}
			value = *s.ConfigVersion
		case "environment_timeout":
			if s.EnvironmentTimeout == nil {
				continue
			}
			value = *s.EnvironmentTimeout
		case "disable_per_environment_manifest":
			if s.DisablePerEnvironmentManifest == nil {
				continue
			}
			value = boolStr(*s.DisablePerEnvironmentManifest)
		case "static_catalogs":
			if s.StaticCatalogs == nil {
				continue
			}
			value = boolStr(*s.StaticCatalogs)
		case "rich_data":
			if s.RichData == nil {
				continue
			}
			value = boolStr(*s.RichData)
		}
		if !valueIsSafe(value) {
			return "", fmt.Errorf("%w: field %s carries a carriage return or line feed", ErrEnvConfInvalid, key)
		}
		lines = append(lines, key+" = "+value)
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}
