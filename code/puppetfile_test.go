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

	unicodeModel := &hostv1.Puppetfile{Modules: []*hostv1.PuppetfileModule{
		{Name: "puppetlabs/ntp-café", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}},
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
