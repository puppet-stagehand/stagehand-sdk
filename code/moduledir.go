package code

import (
	"regexp"
	"strconv"
	"strings"
)

// ModuledirMaxLen is the longest accepted moduledir value, in characters.
const ModuledirMaxLen = 64

// reModuledirSegment is the ASCII allow-list for a moduledir value: one plain
// folder name. The first-character class already refuses a leading "." or "-",
// so ".", "..", hidden names and option-like names fail; no "/" or "\" is in
// the set, so absolute and nested values fail.
var reModuledirSegment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// moduledirFix is the fix line every refusal carries. It also tells an author
// whose environment already stores a now-invalid value how to recover (D-17).
const moduledirFix = "set moduledir to one plain folder name such as modules (letters, digits, _ . and -; no /, " +
	"not starting with . or -, at most 64 characters); an environment that already stores a value like this must " +
	"change it or clear it with SetModuledir (an empty value clears it) before other Puppetfile writes succeed"

// ModuledirError is returned by ValidateModuledir for a value that is not one
// safe relative path segment. It satisfies errors.Is(err, ErrPuppetfileInvalid)
// so every existing mapping of that sentinel keeps working.
type ModuledirError struct {
	// Value is the refused input, verbatim; it is never rewritten.
	Value string
	// Reason says why the value was refused.
	Reason string
	// Fix tells the author how to correct or recover.
	Fix string
}

// Error implements error.
func (e *ModuledirError) Error() string {
	return "puppetfile: invalid module: moduledir " + strconv.Quote(e.Value) + ": " + e.Reason
}

// Is reports a match against ErrPuppetfileInvalid.
func (e *ModuledirError) Is(target error) bool { return target == ErrPuppetfileInvalid }

// ValidateModuledir is the single moduledir rule (FND-02). It admits only one
// plain relative folder name and is called by the SetModuledir setter, the
// RenderPuppetfile sink and the lenient import reader; no second check exists.
//
// r10k 5.0.3 and g10k 0.10.0 install into, and purge unmanaged files from,
// whatever directory moduledir names, including one outside the environment
// when the value is absolute (r10k) or contains ".." (both), so anything but a
// single safe segment is refused. The empty string is refused as a value to
// render; callers that mean "no moduledir line" must not call this at all.
//
// A refusal never rewrites, trims or normalises the input.
func ValidateModuledir(s string) error {
	if reModuledirSegment.MatchString(s) {
		return nil
	}
	return &ModuledirError{Value: s, Reason: moduledirReason(s), Fix: moduledirFix}
}

// moduledirReason names the first rule s breaks, in a fixed order.
func moduledirReason(s string) string {
	switch {
	case s == "":
		return "is empty"
	case s[0] == '/' || s[0] == '\\':
		return "is an absolute path"
	case strings.ContainsAny(s, `/\`):
		return "contains a path separator"
	case s == "." || s == "..":
		return "is a parent or current directory reference"
	case s[0] == '.':
		return "is hidden (starts with .)"
	case s[0] == '-':
		return "looks like a command-line option (starts with -)"
	case len(s) > ModuledirMaxLen:
		return "is longer than 64 characters"
	default:
		return "contains a character outside A-Z a-z 0-9 _ . -"
	}
}
