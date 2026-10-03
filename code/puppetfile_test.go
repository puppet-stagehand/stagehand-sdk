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
