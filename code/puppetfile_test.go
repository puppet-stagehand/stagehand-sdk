package code

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

func TestPuppetfile_ParseForgeModules(t *testing.T) {
	cases := []struct {
		name string
		text string
		want *hostv1.Puppetfile
	}{
		{
			name: "bare",
			text: "mod 'puppetlabs/ntp'\n",
			want: &hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{
				{Name: "puppetlabs/ntp", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}},
			}},
		},
		{
			name: "pinned",
			text: "mod 'puppetlabs/apache', '0.10.0'\n",
			want: &hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{
				{Name: "puppetlabs/apache", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "0.10.0"}}},
			}},
		},
		{
			name: "latest",
			text: "mod 'puppetlabs/stdlib', :latest\n",
			want: &hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{
				{Name: "puppetlabs/stdlib", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Latest: true}}},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePuppetfile(tc.text)
			if err != nil {
				t.Fatalf("ParsePuppetfile(%q) error = %v", tc.text, err)
			}
			if !proto.Equal(got, tc.want) {
				t.Fatalf("ParsePuppetfile(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestPuppetfile_ParseModuledir(t *testing.T) {
	t.Run("single_quote", func(t *testing.T) {
		got, err := ParsePuppetfile("moduledir 'thirdparty'\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Moduledir != "thirdparty" {
			t.Fatalf("Moduledir = %q, want thirdparty", got.Moduledir)
		}
	})
	t.Run("double_quote", func(t *testing.T) {
		got, err := ParsePuppetfile(`moduledir "thirdparty"` + "\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Moduledir != "thirdparty" {
			t.Fatalf("Moduledir = %q, want thirdparty", got.Moduledir)
		}
	})
	t.Run("duplicate_is_error", func(t *testing.T) {
		_, err := ParsePuppetfile("moduledir 'a'\nmoduledir 'b'\n")
		if !errors.Is(err, ErrPuppetfileParse) {
			t.Fatalf("error = %v, want wrapping ErrPuppetfileParse", err)
		}
	})
	t.Run("render_moduledir_blank_line", func(t *testing.T) {
		model := &hostv1.Puppetfile{Moduledir: "thirdparty", Modules: []*hostv1.PuppetfileModule{
			{Name: "puppetlabs/ntp", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}},
		}}
		got, err := RenderPuppetfile(model)
		if err != nil {
			t.Fatalf("RenderPuppetfile error: %v", err)
		}
		want := "moduledir 'thirdparty'\n\nmod 'puppetlabs/ntp'\n"
		if got != want {
			t.Fatalf("RenderPuppetfile = %q, want %q", got, want)
		}
	})
	t.Run("render_moduledir_only_single_trailing_newline", func(t *testing.T) {
		got, err := RenderPuppetfile(&hostv1.Puppetfile{Moduledir: "thirdparty"})
		if err != nil {
			t.Fatalf("RenderPuppetfile error: %v", err)
		}
		want := "moduledir 'thirdparty'\n"
		if got != want {
			t.Fatalf("RenderPuppetfile = %q, want %q", got, want)
		}
	})
}

func TestPuppetfile_ParseEmptyAndCommentsOnly(t *testing.T) {
	for _, text := range []string{"", "# just a comment\n\n# another\n"} {
		got, err := ParsePuppetfile(text)
		if err != nil {
			t.Fatalf("ParsePuppetfile(%q) error = %v", text, err)
		}
		if len(got.Modules) != 0 || got.Moduledir != "" {
			t.Fatalf("ParsePuppetfile(%q) = %+v, want zero-value", text, got)
		}
	}

	t.Run("render_empty_model_returns_empty_string", func(t *testing.T) {
		got, err := RenderPuppetfile(&hostv1.Puppetfile{})
		if err != nil {
			t.Fatalf("RenderPuppetfile error: %v", err)
		}
		if got != "" {
			t.Fatalf("RenderPuppetfile(empty) = %q, want empty string", got)
		}
	})
}

func TestPuppetfile_ParseOrderAndAdjacentNames(t *testing.T) {
	text := "mod 'puppetlabs/ntp'\nmod 'puppetlabs/apache'\nmod 'puppetlabs/apache_extra'\nmod 'puppetlabs/stdlib'\n"
	got, err := ParsePuppetfile(text)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantNames := []string{"puppetlabs/ntp", "puppetlabs/apache", "puppetlabs/apache_extra", "puppetlabs/stdlib"}
	if len(got.Modules) != len(wantNames) {
		t.Fatalf("got %d modules, want %d", len(got.Modules), len(wantNames))
	}
	for i, name := range wantNames {
		if got.Modules[i].Name != name {
			t.Fatalf("Modules[%d].Name = %q, want %q", i, got.Modules[i].Name, name)
		}
	}
}

func TestPuppetfile_ParseQuoteStyleAndUnicode(t *testing.T) {
	single, err := ParsePuppetfile("mod 'puppetlabs/ntp', '1.0.0'\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	double, err := ParsePuppetfile(`mod "puppetlabs/ntp", "1.0.0"` + "\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !proto.Equal(single, double) {
		t.Fatalf("single-quoted and double-quoted parses differ: %v vs %v", single, double)
	}

	// Forge module names are validated against an ASCII slug pattern
	// (ValidateModule, Task 2), so the unicode round-trip case uses a
	// Git-sourced module, whose name carries no such constraint.
	unicodeModel := &hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{
		{Name: "ntp-café", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/ntp-café.git"}}},
	}}
	rendered, err := RenderPuppetfile(unicodeModel)
	if err != nil {
		t.Fatalf("RenderPuppetfile error: %v", err)
	}
	reparsed, err := ParsePuppetfile(rendered)
	if err != nil {
		t.Fatalf("ParsePuppetfile(rendered) error: %v", err)
	}
	if !proto.Equal(unicodeModel, reparsed) {
		t.Fatalf("unicode round-trip mismatch: rendered %q, reparsed %v", rendered, reparsed)
	}
}

func TestPuppetfile_ParseRejectsUnknownAttribute(t *testing.T) {
	_, err := ParsePuppetfile("mod 'puppetlabs/apache',\n  :type => 'forge'\n")
	if !errors.Is(err, ErrPuppetfileParse) {
		t.Fatalf("error = %v, want wrapping ErrPuppetfileParse", err)
	}
	if !strings.Contains(err.Error(), "type") {
		t.Fatalf("error = %v, want it to name the attribute", err)
	}
}

func TestPuppetfile_ParseGitModules(t *testing.T) {
	cases := []struct {
		name string
		text string
		want *hostv1.GitSource
	}{
		{
			name: "no_ref_attribute",
			text: "mod 'apache',\n  :git => 'https://github.com/puppetlabs/puppetlabs-apache'\n",
			want: &hostv1.GitSource{Url: "https://github.com/puppetlabs/puppetlabs-apache"},
		},
		{
			name: "ref",
			text: "mod 'apache',\n  :git => 'url',\n  :ref => 'docs_experiment'\n",
			want: &hostv1.GitSource{Url: "url", RefKind: &hostv1.GitSource_Ref{Ref: "docs_experiment"}},
		},
		{
			name: "tag",
			text: "mod 'apache',\n  :git => 'url',\n  :tag => '0.9.0'\n",
			want: &hostv1.GitSource{Url: "url", RefKind: &hostv1.GitSource_Tag{Tag: "0.9.0"}},
		},
		{
			name: "branch",
			text: "mod 'apache',\n  :git => 'url',\n  :branch => 'docs_experiment'\n",
			want: &hostv1.GitSource{Url: "url", RefKind: &hostv1.GitSource_Branch{Branch: "docs_experiment"}},
		},
		{
			name: "commit",
			text: "mod 'apache',\n  :git => 'url',\n  :commit => '83401079053dca11d61945bd9beef9ecf7576cbf'\n",
			want: &hostv1.GitSource{Url: "url", RefKind: &hostv1.GitSource_Commit{Commit: "83401079053dca11d61945bd9beef9ecf7576cbf"}},
		},
		{
			name: "control_branch",
			text: "mod 'apache',\n  :git => 'url',\n  :branch => :control_branch\n",
			want: &hostv1.GitSource{Url: "url", RefKind: &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}}},
		},
		{
			name: "default_branch_no_ref_arm",
			text: "mod 'apache',\n  :git => 'url',\n  :default_branch => 'main'\n",
			want: &hostv1.GitSource{Url: "url", DefaultBranch: "main"},
		},
		{
			name: "control_branch_with_default_branch",
			text: "mod 'apache',\n  :git => 'url',\n  :branch => :control_branch,\n  :default_branch => 'main'\n",
			want: &hostv1.GitSource{Url: "url", RefKind: &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}}, DefaultBranch: "main"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePuppetfile(tc.text)
			if err != nil {
				t.Fatalf("ParsePuppetfile(%q) error = %v", tc.text, err)
			}
			want := &hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{
				{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: tc.want}},
			}}
			if !proto.Equal(got, want) {
				t.Fatalf("ParsePuppetfile(%q) = %v, want %v", tc.text, got, want)
			}
		})
	}
}

func TestPuppetfile_ParseRejectsTwoRefSelectors(t *testing.T) {
	cases := []string{
		"mod 'apache',\n  :git => 'url',\n  :tag => '0.9.0',\n  :branch => 'main'\n",
		"mod 'apache',\n  :git => 'url',\n  :ref => 'x',\n  :commit => 'y'\n",
	}
	for _, text := range cases {
		_, err := ParsePuppetfile(text)
		if !errors.Is(err, ErrPuppetfileParse) {
			t.Fatalf("ParsePuppetfile(%q) error = %v, want wrapping ErrPuppetfileParse", text, err)
		}
	}
}

// TestPuppetfile_ParseRejectsLeftoverText pins WR-01: text after the git
// attributes that no recognised `:key => value` pair accounts for is refused by
// strict parsing instead of being silently dropped (the parser/reality
// differential NEW-2 exploits).
func TestPuppetfile_ParseRejectsLeftoverText(t *testing.T) {
	cases := []string{
		"mod 'a',\n  :git => 'https://example.com/a.git' + system('x')\n",
		"mod 'a',\n  :git => 'https://example.com/a.git' garbage\n",
		"mod 'a', :git => 'https://example.com/a.git', :tag => 'v1' trailing\n",
	}
	for _, text := range cases {
		_, err := ParsePuppetfile(text)
		if !errors.Is(err, ErrPuppetfileParse) {
			t.Fatalf("ParsePuppetfile(%q) error = %v, want wrapping ErrPuppetfileParse", text, err)
		}
		if !strings.Contains(err.Error(), "unrecognized text after git attributes") {
			t.Fatalf("ParsePuppetfile(%q) error = %v, want it to name the leftover text", text, err)
		}
		if !strings.Contains(err.Error(), "line 1:") {
			t.Fatalf("ParsePuppetfile(%q) error = %v, want a line 1 prefix", text, err)
		}
	}
}

// TestPuppetfile_ParseRejectsDoubleQuotedInterpolation pins WR-01: a
// double-quoted value Ruby would interpolate is refused by strict parsing, while
// the same text in single quotes (literal in Ruby) and a plain double-quoted
// value still parse.
func TestPuppetfile_ParseRejectsDoubleQuotedInterpolation(t *testing.T) {
	refused := []string{
		"mod 'a', :git => \"https://x/#{`id`}\", :tag => 'v1'\n",
		"mod 'a', :git => 'https://example.com/a.git', :tag => \"v#{VERSION}\"\n",
		"mod 'a', :git => 'https://example.com/a.git', :tag => \"v#$X\"\n",
		"mod 'a', :git => 'https://example.com/a.git', :ref => \"v#@x\"\n",
		"moduledir \"#{ENV_DIR}\"\n",
	}
	for _, text := range refused {
		_, err := ParsePuppetfile(text)
		if !errors.Is(err, ErrPuppetfileParse) {
			t.Fatalf("ParsePuppetfile(%q) error = %v, want wrapping ErrPuppetfileParse", text, err)
		}
		if !strings.Contains(err.Error(), "interpolation") {
			t.Fatalf("ParsePuppetfile(%q) error = %v, want it to name interpolation", text, err)
		}
	}
	accepted := []string{
		"mod 'a', :git => 'https://example.com/a.git', :tag => 'v#{VERSION}'\n",
		"mod 'a', :git => \"https://example.com/a.git\", :tag => \"v1\"\n",
		"moduledir 'a#{b}'\n",
		"moduledir \"plain\"\n",
	}
	for _, text := range accepted {
		if _, err := ParsePuppetfile(text); err != nil {
			t.Fatalf("ParsePuppetfile(%q) must still parse: %v", text, err)
		}
	}
}

// TestPuppetfile_ParseRejectsRepeatedGitAttribute pins T-12.1-09: a repeated
// :git or :default_branch is refused rather than last-wins, because an
// operator reading the file sees the first value while Ruby evaluates the last.
func TestPuppetfile_ParseRejectsRepeatedGitAttribute(t *testing.T) {
	cases := []struct {
		text string
		key  string
	}{
		{"mod 'a', :git => 'https://example.com/a.git', :git => 'https://example.com/b.git'\n", "git"},
		{"mod 'a', :git => 'https://example.com/a.git', :default_branch => 'main', :default_branch => 'dev'\n", "default_branch"},
	}
	for _, tc := range cases {
		_, err := ParsePuppetfile(tc.text)
		if !errors.Is(err, ErrPuppetfileParse) {
			t.Fatalf("ParsePuppetfile(%q) error = %v, want wrapping ErrPuppetfileParse", tc.text, err)
		}
		if !strings.Contains(err.Error(), "repeated :"+tc.key) {
			t.Fatalf("ParsePuppetfile(%q) error = %v, want it to name repeated :%s", tc.text, err, tc.key)
		}
		if !strings.Contains(err.Error(), "line 1:") {
			t.Fatalf("ParsePuppetfile(%q) error = %v, want a line 1 prefix", tc.text, err)
		}
	}
	// One :git and one :default_branch still parse.
	if _, err := ParsePuppetfile("mod 'a', :git => 'https://example.com/a.git', :default_branch => 'main'\n"); err != nil {
		t.Fatalf("single :git and :default_branch must still parse: %v", err)
	}
}

func TestPuppetfile_RenderControlBranchIsABareSymbol(t *testing.T) {
	model := &hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{
		{Name: "profiles", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{
			Url:           "git@git.example.com:puppet/profiles.git",
			RefKind:       &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}},
			DefaultBranch: "main",
		}}},
	}}
	got, err := RenderPuppetfile(model)
	if err != nil {
		t.Fatalf("RenderPuppetfile error: %v", err)
	}
	if !strings.Contains(got, ":branch => :control_branch") {
		t.Fatalf("RenderPuppetfile output %q does not contain the bare-symbol control-branch form", got)
	}
	if strings.Contains(got, ":control_branch =>") {
		t.Fatalf("RenderPuppetfile output %q emits control_branch as a key, not a value (Pitfall 2)", got)
	}
	reparsed, err := ParsePuppetfile(got)
	if err != nil {
		t.Fatalf("ParsePuppetfile(rendered) error: %v", err)
	}
	if !proto.Equal(model, reparsed) {
		t.Fatalf("control-branch round-trip mismatch: rendered %q, reparsed %v", got, reparsed)
	}
}

// TestPuppetfile_RenderRejectsUnsafeModuledir covers the fifth sink: Moduledir
// is the one rendered value that never passes through ValidateModule (the
// ungated SetModuledir stores a bare string), so RenderPuppetfile itself must
// refuse an unsafe one and return no text. Path shape is deliberately not
// policed (SD-6): absolute paths and `..` are accepted.
func TestPuppetfile_RenderRejectsUnsafeModuledir(t *testing.T) {
	rejected := []struct {
		name      string
		moduledir string
	}{
		{"moduledir_injects_mod_line", "x'\nmod 'puppetlabs-stdlib', '99.0.0'\n#"},
		{"moduledir_trailing_backslash", `a\`},
		{"moduledir_double_quote", `a"b`},
		{"moduledir_nul", "a\x00b"},
		{"moduledir_paragraph_separator", "a\u2029b"},
		{"moduledir_invalid_utf8", "a\xffb"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderPuppetfile(&hostv1.Puppetfile{Moduledir: tc.moduledir})
			if !errors.Is(err, ErrPuppetfileInvalid) {
				t.Fatalf("RenderPuppetfile(moduledir=%q) error = %v, want wrapping ErrPuppetfileInvalid", tc.moduledir, err)
			}
			if len(got) != 0 {
				t.Fatalf("RenderPuppetfile(moduledir=%q) returned text %q, want the empty string", tc.moduledir, got)
			}
		})
	}

	for _, v := range []string{"thirdparty", "modules/third", "/srv/modules", "../up"} {
		t.Run("accepts_"+v, func(t *testing.T) {
			got, err := RenderPuppetfile(&hostv1.Puppetfile{Moduledir: v})
			if err != nil {
				t.Fatalf("RenderPuppetfile(moduledir=%q) unexpected error = %v", v, err)
			}
			if first, _, _ := strings.Cut(got, "\n"); first != "moduledir '"+v+"'" {
				t.Fatalf("RenderPuppetfile(moduledir=%q) first line = %q, want %q", v, first, "moduledir '"+v+"'")
			}
		})
	}

	for _, p := range []*hostv1.Puppetfile{nil, {}} {
		got, err := RenderPuppetfile(p)
		if err != nil || got != "" {
			t.Fatalf("RenderPuppetfile(%v) = (%q, %v), want (\"\", nil)", p, got, err)
		}
	}
}

// TestPuppetfile_CheckRenderRoundTripDetectsDivergence pins the render-time
// self-check (SD-3): a text/model pair that does not strict-parse to the same
// moduledir and modules is refused, while Environment, which ParsePuppetfile
// never sets, is ignored (12.1-RESEARCH.md Pitfall 5).
func TestPuppetfile_CheckRenderRoundTripDetectsDivergence(t *testing.T) {
	divergent := []struct {
		name string
		out  string
		p    *hostv1.Puppetfile
	}{
		{"text_has_module_model_has_none", "mod 'puppetlabs/ntp'\n", &hostv1.Puppetfile{}},
		{"moduledir_differs", "moduledir 'a'\n", &hostv1.Puppetfile{Moduledir: "b"}},
		{"module_version_differs", "mod 'puppetlabs/ntp', '1.0.0'\n", &hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{forgeModuleVersioned("2.0.0")}}},
		{"text_does_not_parse", "this is not a puppetfile\n", &hostv1.Puppetfile{}},
	}
	for _, tc := range divergent {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkRenderRoundTrip(tc.out, tc.p); !errors.Is(err, ErrPuppetfileInvalid) {
				t.Fatalf("checkRenderRoundTrip(%q, %v) error = %v, want wrapping ErrPuppetfileInvalid", tc.out, tc.p, err)
			}
		})
	}

	t.Run("environment_is_ignored", func(t *testing.T) {
		p := &hostv1.Puppetfile{Environment: "prod", Moduledir: "thirdparty", Modules: []*hostv1.PuppetfileModule{forgeModuleVersioned("1.0.0")}}
		out, err := RenderPuppetfile(p)
		if err != nil {
			t.Fatalf("RenderPuppetfile error: %v", err)
		}
		if err := checkRenderRoundTrip(out, p); err != nil {
			t.Fatalf("checkRenderRoundTrip(%q) with Environment set = %v, want nil", out, err)
		}
	})
}

// gitModuleNamed builds a Git-sourced module with a benign URL so a test can
// vary only the name.
func gitModuleNamed(name string) *hostv1.PuppetfileModule {
	return &hostv1.PuppetfileModule{Name: name, Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/a.git"}}}
}

// TestPuppetfile_RejectsInjectedModuleText is the NEW-2 end-to-end slice: the
// milestone audit's headline payload, a Git module whose Name carries extra
// `mod` statements, must be refused by ValidateModule and must never become
// text out of RenderPuppetfile (GOV-02).
func TestPuppetfile_RejectsInjectedModuleText(t *testing.T) {
	hostile := "zz'\nmod 'puppetlabs-stdlib', '99.0.0'\nmod 'qq"
	mod := gitModuleNamed(hostile)

	if err := ValidateModule(mod); !errors.Is(err, ErrPuppetfileInvalid) {
		t.Fatalf("ValidateModule(hostile git name) error = %v, want wrapping ErrPuppetfileInvalid", err)
	}

	got, err := RenderPuppetfile(&hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{mod}})
	if !errors.Is(err, ErrPuppetfileInvalid) {
		t.Fatalf("RenderPuppetfile(hostile git name) error = %v, want wrapping ErrPuppetfileInvalid", err)
	}
	if len(got) != 0 {
		t.Fatalf("RenderPuppetfile(hostile git name) returned text %q, want the empty string (no partial render)", got)
	}
}

// TestPuppetfile_ScalarSafe pins the safe-scalar predicate behind every
// free-form rendered value: anything that could end a single-quoted Ruby
// literal or start a new line is unsafe.
func TestPuppetfile_ScalarSafe(t *testing.T) {
	unsafe := map[string]string{
		"single_quote":        "a'b",
		"double_quote":        `a"b`,
		"backslash":           `a\b`,
		"trailing_backslash":  `a\`,
		"newline":             "a\nb",
		"carriage_return":     "a\rb",
		"tab":                 "a\tb",
		"vertical_tab":        "a\vb",
		"form_feed":           "a\fb",
		"nul":                 "a\x00b",
		"c1_next_line":        "a\u0085b",
		"line_separator":      "a\u2028b",
		"paragraph_separator": "a\u2029b",
		"invalid_utf8":        "a\xffb",
	}
	for name, v := range unsafe {
		if scalarSafe(v) {
			t.Errorf("scalarSafe(%s = %q) = true, want false", name, v)
		}
	}
	for _, v := range []string{"", "plain", "a b", "release/1.x", "https://example.com/frag.git#branch", "café", "a`b"} {
		if !scalarSafe(v) {
			t.Errorf("scalarSafe(%q) = false, want true", v)
		}
	}
}

// forgeModuleVersioned builds a Forge-sourced module so a test can vary only
// the version.
func forgeModuleVersioned(version string) *hostv1.PuppetfileModule {
	return &hostv1.PuppetfileModule{Name: "puppetlabs/ntp", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: version}}}
}

// gitModuleWithURL builds a Git-sourced module so a test can vary only the URL.
func gitModuleWithURL(url string) *hostv1.PuppetfileModule {
	return &hostv1.PuppetfileModule{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: url}}}
}

// gitModuleWithRef builds a Git-sourced module carrying one ref_kind arm. key
// is the DSL spelling (ref, tag, branch, commit) or "default_branch"; the
// default_branch case sets no ref arm.
func gitModuleWithRef(key, value string) *hostv1.PuppetfileModule {
	gs := &hostv1.GitSource{Url: "https://example.com/a.git"}
	switch key {
	case "ref":
		gs.RefKind = &hostv1.GitSource_Ref{Ref: value}
	case "tag":
		gs.RefKind = &hostv1.GitSource_Tag{Tag: value}
	case "branch":
		gs.RefKind = &hostv1.GitSource_Branch{Branch: value}
	case "commit":
		gs.RefKind = &hostv1.GitSource_Commit{Commit: value}
	case "default_branch":
		gs.DefaultBranch = value
	}
	return &hostv1.PuppetfileModule{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: gs}}
}

// TestPuppetfile_ValidateModuleRefSelectors is the full vector x field matrix
// for the five ref-like values (:ref, :tag, :branch, :commit,
// :default_branch): every hostile value is refused on every field, every
// real-world value is accepted on every field, and control_branch, which
// carries no string, is exempt.
func TestPuppetfile_ValidateModuleRefSelectors(t *testing.T) {
	keys := []string{"ref", "tag", "branch", "commit", "default_branch"}
	hostile := map[string]string{
		"injects_mod_line":  "x'\nmod 'puppetlabs-stdlib', '99.0.0'\n#",
		"injects_statement": "x'\nsystem('id')\n#",
		"leading_hyphen":    "-x",
		"whitespace":        "a b",
		"backslash":         `a\b`,
		"too_long":          strings.Repeat("a", 256),
	}
	for _, key := range keys {
		for name, v := range hostile {
			t.Run(key+"_"+name, func(t *testing.T) {
				if err := ValidateModule(gitModuleWithRef(key, v)); !errors.Is(err, ErrPuppetfileInvalid) {
					t.Fatalf("ValidateModule(%s=%q) error = %v, want wrapping ErrPuppetfileInvalid", key, v, err)
				}
			})
		}
		for _, v := range []string{"main", "release/1.x", "v1.2.3", "docs_experiment", "83401079053dca11d61945bd9beef9ecf7576cbf"} {
			t.Run(key+"_accepts_"+v, func(t *testing.T) {
				if err := ValidateModule(gitModuleWithRef(key, v)); err != nil {
					t.Fatalf("ValidateModule(%s=%q) unexpected error = %v", key, v, err)
				}
			})
		}
	}
	control := &hostv1.PuppetfileModule{Name: "profiles", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{
		Url:     "https://example.com/a.git",
		RefKind: &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}},
	}}}
	if err := ValidateModule(control); err != nil {
		t.Fatalf("ValidateModule(control_branch) unexpected error = %v", err)
	}
}

func TestPuppetfile_ValidateModule(t *testing.T) {
	invalid := []struct {
		name string
		mod  *hostv1.PuppetfileModule
	}{
		{"empty_name", &hostv1.PuppetfileModule{Name: "", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}}},
		{"forge_version_and_latest", &hostv1.PuppetfileModule{Name: "puppetlabs/apache", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "1.0.0", Latest: true}}}},
		{"forge_no_namespace", &hostv1.PuppetfileModule{Name: "apache", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}}},
		{"git_empty_url", &hostv1.PuppetfileModule{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{}}}},
		{"git_url_with_space", &hostv1.PuppetfileModule{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/a b.git"}}}},
		{"git_url_with_newline", &hostv1.PuppetfileModule{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/a\nb.git"}}}},
		{"git_url_command_transport", &hostv1.PuppetfileModule{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "ext::sh -c 'touch pwned'"}}}},
		{"git_url_proxycommand", &hostv1.PuppetfileModule{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "ssh://-oProxyCommand=touch pwned/repo.git"}}}},
		{"git_name_injects_mod_lines", gitModuleNamed("zz'\nmod 'puppetlabs-stdlib', '99.0.0'\nmod 'qq")},
		{"git_name_with_slash", gitModuleNamed("ns/name")},
		{"git_name_dot", gitModuleNamed(".")},
		{"git_name_dotdot", gitModuleNamed("..")},
		{"git_name_leading_hyphen", gitModuleNamed("-x")},
		{"git_name_with_backslash", gitModuleNamed(`a\b`)},
		{"git_name_with_double_quote", gitModuleNamed(`a"b`)},
		{"git_name_invalid_utf8", gitModuleNamed("a\xffb")},
		{"forge_version_injects_statement", forgeModuleVersioned("1.0'\nsystem('id')\n#")},
		{"forge_version_trailing_backslash", forgeModuleVersioned(`1.0\`)},
		{"forge_version_double_quote", forgeModuleVersioned(`1.0"`)},
		{"forge_version_with_space", forgeModuleVersioned(">= 1.0")},
		{"forge_version_invalid_utf8", forgeModuleVersioned("1.0\xff")},
		{"forge_version_too_long", forgeModuleVersioned("1" + strings.Repeat("0", 128))},
		{"forge_name_trailing_newline", &hostv1.PuppetfileModule{Name: "puppetlabs/ntp\n", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}}},
		{"git_url_expression_breakout", gitModuleWithURL("https://a/'+`id`+'")},
		{"git_url_vertical_tab_form_feed_nul", gitModuleWithURL("https://a/\v\fb\x00")},
		{"git_url_line_separator", gitModuleWithURL("https://a/\u2028b")},
		{"git_url_invalid_utf8", gitModuleWithURL("https://a/\xffb")},
		{"git_url_backslash", gitModuleWithURL(`https://a/a\b`)},
		{"git_ref_injects_mod_line", gitModuleWithRef("ref", "x'\nmod 'puppetlabs-stdlib', '99.0.0'\n#")},
		{"git_tag_injects_mod_line", gitModuleWithRef("tag", "x'\nmod 'puppetlabs-stdlib', '99.0.0'\n#")},
		{"git_branch_injects_mod_line", gitModuleWithRef("branch", "x'\nmod 'puppetlabs-stdlib', '99.0.0'\n#")},
		{"git_commit_injects_mod_line", gitModuleWithRef("commit", "x'\nmod 'puppetlabs-stdlib', '99.0.0'\n#")},
		{"git_default_branch_injects_statement", gitModuleWithRef("default_branch", "x'\nsystem('id')\n#")},
		{"git_ref_leading_hyphen", gitModuleWithRef("ref", "-x")},
		{"git_ref_with_space", gitModuleWithRef("ref", "a b")},
		{"git_ref_too_long", gitModuleWithRef("ref", strings.Repeat("a", 256))},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateModule(tc.mod)
			if !errors.Is(err, ErrPuppetfileInvalid) {
				t.Fatalf("ValidateModule(%+v) error = %v, want wrapping ErrPuppetfileInvalid", tc.mod, err)
			}
		})
	}

	valid := []*hostv1.PuppetfileModule{
		{Name: "puppetlabs/apache", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}},
		{Name: "puppetlabs-apache", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}},
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://github.com/puppetlabs/puppetlabs-apache"}}},
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "git://github.com/puppetlabs/puppetlabs-apache"}}},
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "ssh://git@github.com/puppetlabs/puppetlabs-apache"}}},
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "file:///var/repos/apache.git"}}},
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "git@git.example.com:puppet/apache.git"}}},
		gitModuleNamed("ntp-café"),
		gitModuleNamed("profiles"),
		gitModuleNamed("apache_docs"),
		gitModuleNamed("abs_git"),
		gitModuleNamed("skip_install_path"),
		gitModuleNamed("conflict"),
		gitModuleNamed("ntp.v2"),
		forgeModuleVersioned("1.0"),
		forgeModuleVersioned("1.0.0"),
		forgeModuleVersioned("1.2.3-rc.1+build.5"),
		forgeModuleVersioned("0.10.0"),
		forgeModuleVersioned("7.0.1"),
		forgeModuleVersioned("9.9.9"),
		forgeModuleVersioned(""),
		gitModuleWithURL("https://example.com/frag.git#branch"),
		gitModuleWithURL("git@git.example.com:puppet/apache.git"),
		gitModuleWithURL("file:///var/repos/apache.git"),
		gitModuleWithURL("git://github.com/puppetlabs/puppetlabs-apache"),
		gitModuleWithURL("ssh://git@github.com/puppetlabs/puppetlabs-apache"),
	}
	for i, mod := range valid {
		if err := ValidateModule(mod); err != nil {
			t.Fatalf("ValidateModule(valid[%d]=%+v) unexpected error = %v", i, mod, err)
		}
	}
}

// roundTripFixtures holds at least ten *hostv1.Puppetfile values covering
// every shape RenderPuppetfile/ParsePuppetfile support, per PF-05.
var roundTripFixtures = []*hostv1.Puppetfile{
	{},
	{Moduledir: "thirdparty"},
	{Modules: []*hostv1.PuppetfileModule{
		{Name: "puppetlabs/ntp", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}},
	}},
	{Modules: []*hostv1.PuppetfileModule{
		{Name: "puppetlabs/apache", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "0.10.0"}}},
	}},
	{Modules: []*hostv1.PuppetfileModule{
		{Name: "puppetlabs/stdlib", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Latest: true}}},
	}},
	{Modules: []*hostv1.PuppetfileModule{
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/apache.git", RefKind: &hostv1.GitSource_Ref{Ref: "docs_experiment"}}}},
	}},
	{Modules: []*hostv1.PuppetfileModule{
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/apache.git", RefKind: &hostv1.GitSource_Tag{Tag: "0.9.0"}}}},
	}},
	{Modules: []*hostv1.PuppetfileModule{
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/apache.git", RefKind: &hostv1.GitSource_Branch{Branch: "docs_experiment"}}}},
	}},
	{Modules: []*hostv1.PuppetfileModule{
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/apache.git", RefKind: &hostv1.GitSource_Commit{Commit: "83401079053dca11d61945bd9beef9ecf7576cbf"}}}},
	}},
	{Modules: []*hostv1.PuppetfileModule{
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/apache.git", RefKind: &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}}}}},
	}},
	{Modules: []*hostv1.PuppetfileModule{
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/apache.git", DefaultBranch: "main"}}},
	}},
	{Modules: []*hostv1.PuppetfileModule{
		{Name: "profiles", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "git@git.example.com:puppet/profiles.git", RefKind: &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}}, DefaultBranch: "main"}}},
	}},
	{
		Moduledir: "thirdparty",
		Modules: []*hostv1.PuppetfileModule{
			{Name: "puppetlabs/ntp", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}},
			{Name: "puppetlabs/apache", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "0.10.0"}}},
			{Name: "puppetlabs/stdlib", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Latest: true}}},
			{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://github.com/puppetlabs/puppetlabs-apache"}}},
			{Name: "concat", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://github.com/puppetlabs/puppetlabs-concat", RefKind: &hostv1.GitSource_Tag{Tag: "0.9.0"}}}},
			{Name: "profiles", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "git@git.example.com:puppet/profiles.git", RefKind: &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}}, DefaultBranch: "main"}}},
		},
	},
}

func TestPuppetfile_RoundTrip(t *testing.T) {
	if len(roundTripFixtures) < 10 {
		t.Fatalf("roundTripFixtures has %d entries, want at least 10", len(roundTripFixtures))
	}
	for i, fixture := range roundTripFixtures {
		rendered, err := RenderPuppetfile(fixture)
		if err != nil {
			t.Fatalf("fixture[%d]: RenderPuppetfile error = %v", i, err)
		}
		reparsed, err := ParsePuppetfile(rendered)
		if err != nil {
			t.Fatalf("fixture[%d]: ParsePuppetfile(rendered=%q) error = %v", i, rendered, err)
		}
		if !proto.Equal(fixture, reparsed) {
			t.Fatalf("fixture[%d]: round-trip mismatch: rendered %q, reparsed %v, want %v", i, rendered, reparsed, fixture)
		}
		rerendered, err := RenderPuppetfile(reparsed)
		if err != nil {
			t.Fatalf("fixture[%d]: second RenderPuppetfile error = %v", i, err)
		}
		if rendered != rerendered {
			t.Fatalf("fixture[%d]: second render not byte-identical:\nfirst:  %q\nsecond: %q", i, rendered, rerendered)
		}
	}
}

func TestPuppetfile_RenderMatchesCanonicalContractExample(t *testing.T) {
	// The multi-shape fixture (index 12) mirrors <render_contract>'s
	// canonical example verbatim.
	fixture := roundTripFixtures[12]
	got, err := RenderPuppetfile(fixture)
	if err != nil {
		t.Fatalf("RenderPuppetfile error: %v", err)
	}
	want := "moduledir 'thirdparty'\n" +
		"\n" +
		"mod 'puppetlabs/ntp'\n" +
		"mod 'puppetlabs/apache', '0.10.0'\n" +
		"mod 'puppetlabs/stdlib', :latest\n" +
		"mod 'apache',\n" +
		"  :git => 'https://github.com/puppetlabs/puppetlabs-apache'\n" +
		"mod 'concat',\n" +
		"  :git => 'https://github.com/puppetlabs/puppetlabs-concat',\n" +
		"  :tag => '0.9.0'\n" +
		"mod 'profiles',\n" +
		"  :git => 'git@git.example.com:puppet/profiles.git',\n" +
		"  :branch => :control_branch,\n" +
		"  :default_branch => 'main'\n"
	if got != want {
		t.Fatalf("RenderPuppetfile canonical example mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestCanonicalModuleName is the one table for Forge module identity (D-05,
// D-08): every accepted spelling of one module folds to the lowercase
// owner-name hyphen form, and anything outside the Forge slug grammar is
// returned unchanged. Host code compares through this function on both the
// Resolve already-present path and the Code overwrite gate, so there is no
// second table for it anywhere else.
func TestCanonicalModuleName(t *testing.T) {
	rows := []struct {
		name string
		in   string
		want string
	}{
		{"slash_form", "puppetlabs/stdlib", "puppetlabs-stdlib"},
		{"hyphen_form", "puppetlabs-stdlib", "puppetlabs-stdlib"},
		{"mixed_case_owner_slash", "PuppetLabs/stdlib", "puppetlabs-stdlib"},
		{"upper_owner_hyphen", "PUPPETLABS-stdlib", "puppetlabs-stdlib"},
		{"empty", "", ""},
		{"single_segment_unchanged", "stdlib", "stdlib"},
		{"multi_separator_unchanged", "a/b/c", "a/b/c"},
		{"upper_module_segment_unchanged", "Puppetlabs/Stdlib", "Puppetlabs/Stdlib"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got := CanonicalModuleName(tc.in)
			if got != tc.want {
				t.Fatalf("CanonicalModuleName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := CanonicalModuleName(got); again != got {
				t.Fatalf("CanonicalModuleName is not idempotent for %q: first %q, second %q", tc.in, got, again)
			}
		})
	}
}

// A valid Forge slug canonicalises to the hyphen form and never contains the
// slash: the slash is the display/wire spelling, kept out of the identity key
// so the two forms of one module cannot compare unequal.
func TestCanonicalModuleNameNeverKeepsSlashForSlugs(t *testing.T) {
	for _, in := range []string{
		"puppetlabs/stdlib", "puppetlabs-stdlib", "PuppetLabs/stdlib",
		"PUPPETLABS-stdlib", "a1/b_2", "A-b",
	} {
		if !forgeSlugOK(in) {
			t.Fatalf("test input %q is not a Forge slug", in)
		}
		if got := CanonicalModuleName(in); strings.Contains(got, "/") {
			t.Fatalf("CanonicalModuleName(%q) = %q, must not contain '/'", in, got)
		}
	}
}
