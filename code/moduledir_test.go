package code

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateModuledir(t *testing.T) {
	accepted := []string{
		"modules", "thirdparty", "vendor", "ext-modules", "m", "_x", "a..b",
		strings.Repeat("a", ModuledirMaxLen),
	}
	for _, v := range accepted {
		t.Run("accepts_"+v, func(t *testing.T) {
			if err := ValidateModuledir(v); err != nil {
				t.Fatalf("ValidateModuledir(%q) = %v, want nil", v, err)
			}
		})
	}

	refused := []struct{ name, value string }{
		{"empty", ""},
		{"absolute", "/srv/modules"},
		{"parent_reference", "../up"},
		{"dot_dot", ".."},
		{"dot", "."},
		{"nested", "a/b"},
		{"trailing_separator", "modules/"},
		{"hidden", ".modules"},
		{"option_like", "-x"},
		{"long_option_like", "--foo"},
		{"too_long_65", strings.Repeat("a", ModuledirMaxLen+1)},
		{"space", "a b"},
		{"drive_prefix", "C:modules"},
		{"backslash", `a\b`},
		{"nul", "a\x00b"},
		{"paragraph_separator", "a b"},
		{"invalid_utf8", "a\xffb"},
		{"non_ascii_letter", "módulos"},
		{"injects_mod_line", "x'\nmod 'puppetlabs-stdlib', '99.0.0'\n#"},
	}
	for _, tc := range refused {
		t.Run("refuses_"+tc.name, func(t *testing.T) {
			err := ValidateModuledir(tc.value)
			var me *ModuledirError
			if !errors.As(err, &me) {
				t.Fatalf("ValidateModuledir(%q) = %v, want a *ModuledirError", tc.value, err)
			}
			if me.Reason == "" {
				t.Errorf("ValidateModuledir(%q): empty Reason", tc.value)
			}
			if !strings.Contains(me.Fix, "SetModuledir") {
				t.Errorf("ValidateModuledir(%q): Fix %q does not mention SetModuledir", tc.value, me.Fix)
			}
			if me.Value != tc.value {
				t.Errorf("ValidateModuledir(%q): Value = %q, the input must be reported verbatim", tc.value, me.Value)
			}
			if !errors.Is(err, ErrPuppetfileInvalid) {
				t.Errorf("ValidateModuledir(%q) error does not wrap ErrPuppetfileInvalid", tc.value)
			}
		})
	}
}

// TestValidateModuledirSubsumesScalarSafe pins that the path-shape rule is a
// strict superset of the older scalar rule: nothing scalarSafe refuses can be
// accepted, so replacing scalarSafe at the sink and the reader loosens nothing.
func TestValidateModuledirSubsumesScalarSafe(t *testing.T) {
	seeds := []string{
		"x'\nmod 'puppetlabs-stdlib', '99.0.0'\n#", `a\`, `a"b`, "a\x00b", "a b", "a b", "a\xffb",
		"x'", "a\"b", "a\vb", "a\fb", "a​b", "a‮b", "a\tb", "a\rb", "a\nb",
	}
	for _, v := range seeds {
		if scalarSafe(v) {
			continue
		}
		if ValidateModuledir(v) == nil {
			t.Errorf("scalarSafe refuses %q but ValidateModuledir accepts it", v)
		}
	}
}
