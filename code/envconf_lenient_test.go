package code

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// TestParseEnvConfLenient is the environment.conf half of the RESEARCH
// catalog, one row per construct. Every finding is a warning: the file is
// never unparseable as a whole.
func TestParseEnvConfLenient(t *testing.T) {
	unrec := ks(FindingEnvConfUnrecognizedKey, sevW)
	malformed := ks(FindingEnvConfMalformedLine, sevW)
	badBool := ks(FindingEnvConfInvalidBoolean, sevW)

	rows := []struct {
		name string
		text string
		want []string
		// settings is the expected result; nil means absent.
		settings *hostv1.EnvironmentSettings
	}{
		{name: "canonical_control_repo_file", text: readBranchFixtureFile(t, "canonical", "environment.conf"),
			settings: &hostv1.EnvironmentSettings{
				Modulepath:    strPtr("site-modules:modules:$basemodulepath"),
				ConfigVersion: strPtr("'scripts/config_version.sh $environmentpath $environment'"),
			}},
		{name: "unrecognised_key_is_skipped_and_the_rest_kept", text: "strict_variables = true\nmodulepath = a:b\n",
			want: []string{unrec}, settings: &hostv1.EnvironmentSettings{Modulepath: strPtr("a:b")}},
		{name: "section_header_is_a_malformed_line_and_keys_under_it_are_kept", text: "[main]\nmodulepath = a:b\n",
			want: []string{malformed}, settings: &hostv1.EnvironmentSettings{Modulepath: strPtr("a:b")}},
		{name: "line_without_equals", text: "just words\nmanifest = site.pp\n",
			want: []string{malformed}, settings: &hostv1.EnvironmentSettings{Manifest: strPtr("site.pp")}},
		{name: "invalid_boolean_disable_per_environment_manifest", text: "disable_per_environment_manifest = yes\nrich_data = true\n",
			want: []string{badBool}, settings: &hostv1.EnvironmentSettings{RichData: boolPtr(true)}},
		{name: "invalid_boolean_static_catalogs", text: "static_catalogs = 1\nmanifest = m\n",
			want: []string{badBool}, settings: &hostv1.EnvironmentSettings{Manifest: strPtr("m")}},
		{name: "invalid_boolean_rich_data", text: "rich_data = maybe\nenvironment_timeout = unlimited\n",
			want: []string{badBool}, settings: &hostv1.EnvironmentSettings{EnvironmentTimeout: strPtr("unlimited")}},
		{name: "valid_booleans_are_case_insensitive", text: "static_catalogs = TRUE\nrich_data = False\n",
			settings: &hostv1.EnvironmentSettings{StaticCatalogs: boolPtr(true), RichData: boolPtr(false)}},
		{name: "value_with_a_bare_carriage_return_leaves_the_field_unset", text: "modulepath = a\rb\nmanifest = m\n",
			want: []string{malformed}, settings: &hostv1.EnvironmentSettings{Manifest: strPtr("m")}},
		{name: "value_that_is_not_utf8_leaves_the_field_unset", text: "modulepath = \xff\xfe\nmanifest = m\n",
			want: []string{malformed}, settings: &hostv1.EnvironmentSettings{Manifest: strPtr("m")}},
		{name: "bom_prefix", text: "\xEF\xBB\xBFmodulepath = a\n",
			settings: &hostv1.EnvironmentSettings{Modulepath: strPtr("a")}},
		{name: "crlf_line_endings", text: "modulepath = a\r\nmanifest = b\r\n",
			settings: &hostv1.EnvironmentSettings{Modulepath: strPtr("a"), Manifest: strPtr("b")}},
		{name: "every_line_a_comment", text: "# a\n# b\n", settings: nil},
		{name: "empty_file", text: "", settings: nil},
		{name: "blank_lines_only", text: "\n  \n\t\n", settings: nil},
		{name: "every_recognised_key_has_an_invalid_value", text: "static_catalogs = x\nrich_data = y\ndisable_per_environment_manifest = z\nmodulepath = a\rb\n",
			want: []string{badBool, badBool, badBool, malformed}, settings: nil},
		{name: "only_unknown_keys", text: "foo = 1\nbar = 2\n", want: []string{unrec, unrec}, settings: nil},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got, fs := ParseEnvConfLenient(tc.text, DefaultImportLimits())
			wantKindSev(t, fs, tc.want...)
			if tc.settings == nil {
				if got != nil {
					t.Fatalf("an all-nil result must be nil so no settings key is written, got %v", got)
				}
				return
			}
			if got == nil || !proto.Equal(got, tc.settings) {
				t.Fatalf("settings = %v, want %v", got, tc.settings)
			}
			// The result can never fail the facet's own round trip.
			if err := settingsReadPath(got); err != nil {
				t.Fatalf("settings fail the render+parse round trip: %v", err)
			}
		})
	}

	t.Run("canonical_file_matches_strict_parse_exactly", func(t *testing.T) {
		text := readBranchFixtureFile(t, "canonical", "environment.conf")
		strict, err := ParseEnvConf(text)
		if err != nil {
			t.Fatalf("test premise broken: %v", err)
		}
		lenient, fs := ParseEnvConfLenient(text, DefaultImportLimits())
		if len(fs) != 0 || !proto.Equal(strict, lenient) {
			t.Fatalf("lenient = %v (%d findings), strict = %v", lenient, len(fs), strict)
		}
	})

	t.Run("constructs_fixture_has_one_example_of_every_row", func(t *testing.T) {
		b, err := os.ReadFile(filepath.Join("testdata", "import", "envconf", "constructs", "environment.conf"))
		if err != nil {
			t.Fatal(err)
		}
		got, fs := ParseEnvConfLenient(string(b), DefaultImportLimits())
		wantKindSev(t, fs, ks(FindingEnvConfUnrecognizedKey, sevW), ks(FindingEnvConfMalformedLine, sevW), ks(FindingEnvConfMalformedLine, sevW), ks(FindingEnvConfInvalidBoolean, sevW))
		if got.GetModulepath() != "site-modules:modules:$basemodulepath" || !strings.Contains(got.GetConfigVersion(), "config_version.sh") || got.StaticCatalogs != nil {
			t.Fatalf("settings = %v", got)
		}
	})

	t.Run("every_finding_is_a_warning_with_a_line_and_excerpt", func(t *testing.T) {
		_, fs := ParseEnvConfLenient("[main]\nfoo = 1\nrich_data = x\n", DefaultImportLimits())
		if len(fs) != 3 {
			t.Fatalf("findings = %v", kindSev(fs))
		}
		for i, f := range fs {
			if f.GetSeverity() != hostv1.ImportFinding_WARNING || f.GetLine() != int32(i+1) || f.GetExcerpt() == "" {
				t.Errorf("finding %d = %v", i, f)
			}
		}
	})
}
