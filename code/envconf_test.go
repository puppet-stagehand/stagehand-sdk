package code

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

func TestEnvConf_ParseAndRender(t *testing.T) {
	t.Run("empty_text_all_nil", func(t *testing.T) {
		got, err := ParseEnvConf("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := &hostv1.EnvironmentSettings{}
		if !proto.Equal(got, want) {
			t.Fatalf("ParseEnvConf(\"\") = %v, want %v", got, want)
		}
	})

	t.Run("comments_and_blank_lines_only", func(t *testing.T) {
		got, err := ParseEnvConf("# a comment\n\n   \n# another\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := &hostv1.EnvironmentSettings{}
		if !proto.Equal(got, want) {
			t.Fatalf("ParseEnvConf(comments-only) = %v, want %v", got, want)
		}
	})

	t.Run("modulepath_trims_whitespace", func(t *testing.T) {
		got, err := ParseEnvConf("  modulepath   =   modules:$basemodulepath  \n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.GetModulepath() != "modules:$basemodulepath" {
			t.Fatalf("Modulepath = %q, want %q", got.GetModulepath(), "modules:$basemodulepath")
		}
	})

	t.Run("static_catalogs_case_insensitive", func(t *testing.T) {
		got, err := ParseEnvConf("static_catalogs = true\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.GetStaticCatalogs() {
			t.Fatalf("StaticCatalogs = false, want true")
		}
		got, err = ParseEnvConf("static_catalogs = FALSE\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.GetStaticCatalogs() {
			t.Fatalf("StaticCatalogs = true, want false")
		}
	})

	t.Run("unrecognized_key_is_error", func(t *testing.T) {
		_, err := ParseEnvConf("bogus_key = 1\n")
		if !errors.Is(err, ErrEnvConfParse) {
			t.Fatalf("error = %v, want wrapping ErrEnvConfParse", err)
		}
		if !strings.Contains(err.Error(), "bogus_key") {
			t.Fatalf("error = %v, want it to name the key", err)
		}
	})

	t.Run("missing_equals_is_error", func(t *testing.T) {
		_, err := ParseEnvConf("not a valid line\n")
		if !errors.Is(err, ErrEnvConfParse) {
			t.Fatalf("error = %v, want wrapping ErrEnvConfParse", err)
		}
	})

	t.Run("empty_right_hand_side_is_written_as_empty_string", func(t *testing.T) {
		got, err := ParseEnvConf("manifest = \n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Manifest == nil {
			t.Fatalf("Manifest = nil, want a non-nil pointer to the empty string")
		}
		if *got.Manifest != "" {
			t.Fatalf("Manifest = %q, want empty string", *got.Manifest)
		}
	})

	t.Run("render_all_nil_returns_empty_string", func(t *testing.T) {
		got, err := RenderEnvConf(&hostv1.EnvironmentSettings{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Fatalf("RenderEnvConf(all-nil) = %q, want empty string", got)
		}
	})

	t.Run("render_two_fields_in_declaration_order", func(t *testing.T) {
		s := &hostv1.EnvironmentSettings{
			Manifest: strPtr("manifests/"),
			RichData: boolPtr(true),
		}
		got, err := RenderEnvConf(s)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "manifest = manifests/\nrich_data = true\n"
		if got != want {
			t.Fatalf("RenderEnvConf = %q, want %q", got, want)
		}
	})
}

func TestEnvConf_UnwrittenFieldsStayNil(t *testing.T) {
	got, err := ParseEnvConf("manifest = manifests/\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Manifest == nil || *got.Manifest != "manifests/" {
		t.Fatalf("Manifest = %v, want pointer to \"manifests/\"", got.Manifest)
	}
	if got.Modulepath != nil {
		t.Fatalf("Modulepath = %v, want nil", got.Modulepath)
	}
	if got.ConfigVersion != nil {
		t.Fatalf("ConfigVersion = %v, want nil", got.ConfigVersion)
	}
	if got.EnvironmentTimeout != nil {
		t.Fatalf("EnvironmentTimeout = %v, want nil", got.EnvironmentTimeout)
	}
	if got.DisablePerEnvironmentManifest != nil {
		t.Fatalf("DisablePerEnvironmentManifest = %v, want nil", got.DisablePerEnvironmentManifest)
	}
	if got.StaticCatalogs != nil {
		t.Fatalf("StaticCatalogs = %v, want nil", got.StaticCatalogs)
	}
	if got.RichData != nil {
		t.Fatalf("RichData = %v, want nil", got.RichData)
	}
}

func TestEnvConf_RejectsNewlineInValue(t *testing.T) {
	cases := []struct {
		name string
		s    *hostv1.EnvironmentSettings
	}{
		{"primary_field_carriage_return", &hostv1.EnvironmentSettings{Manifest: strPtr("manifests/\rextra")}},
		{"primary_field_line_feed", &hostv1.EnvironmentSettings{Manifest: strPtr("manifests/\nextra")}},
		{"environment_timeout_carriage_return", &hostv1.EnvironmentSettings{EnvironmentTimeout: strPtr("5m\rextra")}},
		{"environment_timeout_line_feed", &hostv1.EnvironmentSettings{EnvironmentTimeout: strPtr("5m\nextra")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderEnvConf(tc.s)
			if !errors.Is(err, ErrEnvConfInvalid) {
				t.Fatalf("error = %v, want wrapping ErrEnvConfInvalid", err)
			}
			if got != "" {
				t.Fatalf("RenderEnvConf returned %q on error, want empty string emitted", got)
			}
		})
	}
}

func TestEnvConf_RoundTrip(t *testing.T) {
	fixtures := []*hostv1.EnvironmentSettings{
		{},
		{Manifest: strPtr("manifests/")},
		{Manifest: strPtr("")},
		{
			DisablePerEnvironmentManifest: boolPtr(false),
			StaticCatalogs:                boolPtr(true),
			RichData:                      boolPtr(true),
		},
		{
			Modulepath:                    strPtr("modules:$basemodulepath"),
			Manifest:                      strPtr("manifests/"),
			ConfigVersion:                 strPtr("scripts/config_version.sh"),
			EnvironmentTimeout:            strPtr("0"),
			DisablePerEnvironmentManifest: boolPtr(false),
			StaticCatalogs:                boolPtr(true),
			RichData:                      boolPtr(true),
		},
	}
	if len(fixtures) < 5 {
		t.Fatalf("fixtures has %d entries, want at least 5", len(fixtures))
	}
	for i, fixture := range fixtures {
		rendered, err := RenderEnvConf(fixture)
		if err != nil {
			t.Fatalf("fixture[%d]: RenderEnvConf error = %v", i, err)
		}
		reparsed, err := ParseEnvConf(rendered)
		if err != nil {
			t.Fatalf("fixture[%d]: ParseEnvConf(rendered=%q) error = %v", i, rendered, err)
		}
		if !proto.Equal(fixture, reparsed) {
			t.Fatalf("fixture[%d]: round-trip mismatch: rendered %q, reparsed %v, want %v", i, rendered, reparsed, fixture)
		}
	}
}
