package code

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// viaStore passes a proposal body through the same structpb conversion the
// Documents store applies, so a round trip is proven against the shape a body
// has when it is read back, not only the shape the builder produced (numbers
// become float64, nested maps become plain maps).
func viaStore(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	st, err := structpb.NewStruct(body)
	if err != nil {
		t.Fatalf("body is not storable: %v", err)
	}
	return st.AsMap()
}

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

func mustTarget(t *testing.T, body map[string]any) OverwriteTarget {
	t.Helper()
	got, err := ParseOverwriteTarget(body)
	if err != nil {
		t.Fatalf("ParseOverwriteTarget: %v", err)
	}
	return got
}

func TestOverwriteBodyRoundTrip(t *testing.T) {
	t.Run("settings", func(t *testing.T) {
		cases := map[string]*hostv1.EnvironmentSettings{
			"all fields unset": {Environment: "production"},
			"some fields set":  {Environment: "production", Modulepath: strp("site:modules"), RichData: boolp(true)},
			"present but empty and false": {
				Environment: "production", Manifest: strp(""), StaticCatalogs: boolp(false),
			},
			"all seven set": {
				Environment: "production", Modulepath: strp("m"), Manifest: strp("s.pp"), ConfigVersion: strp("v"),
				EnvironmentTimeout: strp("5m"), DisablePerEnvironmentManifest: boolp(true),
				StaticCatalogs: boolp(true), RichData: boolp(false),
			},
		}
		for name, in := range cases {
			t.Run(name, func(t *testing.T) {
				body, err := OverwriteBodyForSettings(in)
				if err != nil {
					t.Fatalf("OverwriteBodyForSettings: %v", err)
				}
				if _, has := body["status"]; has {
					t.Fatal("a builder must never write a status key")
				}
				want := OverwriteTarget{Environment: "production", Resource: OverwriteResourceSettings}
				if got := mustTarget(t, viaStore(t, body)); got != want {
					t.Fatalf("target = %+v, want %+v", got, want)
				}
				out, err := OverwritePayloadSettings(viaStore(t, body))
				if err != nil {
					t.Fatalf("OverwritePayloadSettings: %v", err)
				}
				if !proto.Equal(in, out) {
					t.Fatalf("round trip changed the record:\n in  %v\n out %v", in, out)
				}
			})
		}

		t.Run("the environment name is not stored twice", func(t *testing.T) {
			body, _ := OverwriteBodyForSettings(&hostv1.EnvironmentSettings{Environment: "production", Modulepath: strp("m")})
			if _, dup := body["payload"].(map[string]any)["environment"]; dup {
				t.Fatalf("payload repeats the environment: %v", body["payload"])
			}
		})

		t.Run("an unset optional field stays unset rather than becoming empty", func(t *testing.T) {
			body, _ := OverwriteBodyForSettings(&hostv1.EnvironmentSettings{Environment: "production", Modulepath: strp("m")})
			out, err := OverwritePayloadSettings(viaStore(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if out.Manifest != nil || out.RichData != nil {
				t.Fatalf("unset fields decoded as present: %+v", out)
			}
		})

		t.Run("builder refuses a missing environment", func(t *testing.T) {
			if _, err := OverwriteBodyForSettings(&hostv1.EnvironmentSettings{}); !errors.Is(err, ErrOverwriteBodyInvalid) {
				t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
			}
			if _, err := OverwriteBodyForSettings(nil); !errors.Is(err, ErrOverwriteBodyInvalid) {
				t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
			}
		})

		t.Run("payload of another resource kind is refused", func(t *testing.T) {
			body, _ := OverwriteBodyForEnvironmentDuplicate("production", "staging")
			if _, err := OverwritePayloadSettings(viaStore(t, body)); !errors.Is(err, ErrOverwriteBodyInvalid) {
				t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
			}
		})
	})

	t.Run("environment duplicate", func(t *testing.T) {
		body, err := OverwriteBodyForEnvironmentDuplicate("production", "staging")
		if err != nil {
			t.Fatalf("OverwriteBodyForEnvironmentDuplicate: %v", err)
		}
		if _, has := body["status"]; has {
			t.Fatal("a builder must never write a status key")
		}
		if _, has := body["payload"]; has {
			t.Fatal("a duplicate proposal approves the operation and freezes no payload")
		}
		want := OverwriteTarget{Environment: "staging", Resource: OverwriteResourceEnvironment, Source: "production"}
		if got := mustTarget(t, viaStore(t, body)); got != want {
			t.Fatalf("target = %+v, want %+v", got, want)
		}

		for _, bad := range [][2]string{{"", "staging"}, {"production", ""}, {"", ""}} {
			if _, err := OverwriteBodyForEnvironmentDuplicate(bad[0], bad[1]); !errors.Is(err, ErrOverwriteBodyInvalid) {
				t.Fatalf("(%q,%q): got %v, want ErrOverwriteBodyInvalid", bad[0], bad[1], err)
			}
		}
	})

	t.Run("hiera level", func(t *testing.T) {
		cases := map[string]struct {
			lvl    *hostv1.HieraLevel
			index  int32
			insert bool
		}{
			"path level replace": {lvl: &hostv1.HieraLevel{Name: "common", Path: "common.yaml", DataHash: "yaml_data"}},
			"paths level insert": {lvl: &hostv1.HieraLevel{Name: "os", Paths: []string{"os/a.yaml", "os/b.yaml"}}, index: 3, insert: true},
			"mapped_paths level": {lvl: &hostv1.HieraLevel{Name: "m", MappedPaths: []string{"facts.a", "x", "n/${x}.yaml"}, Datadir: "d"}, index: 0, insert: true},
			"glob level":         {lvl: &hostv1.HieraLevel{Name: "g", Glob: "conf.d/*.yaml"}},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				body, err := OverwriteBodyForHieraLevel("prod", tc.lvl, tc.index, tc.insert)
				if err != nil {
					t.Fatalf("OverwriteBodyForHieraLevel: %v", err)
				}
				if _, has := body["status"]; has {
					t.Fatal("a builder must never write a status key")
				}
				want := OverwriteTarget{Environment: "prod", Resource: OverwriteResourceHieraLevel, Name: tc.lvl.GetName()}
				if got := mustTarget(t, viaStore(t, body)); got != want {
					t.Fatalf("target = %+v, want %+v", got, want)
				}
				lvl, index, insert, err := OverwritePayloadHieraLevel(viaStore(t, body))
				if err != nil {
					t.Fatalf("OverwritePayloadHieraLevel: %v", err)
				}
				if !proto.Equal(lvl, tc.lvl) || index != tc.index || insert != tc.insert {
					t.Fatalf("round trip changed the write: got (%v,%d,%v) want (%v,%d,%v)", lvl, index, insert, tc.lvl, tc.index, tc.insert)
				}
			})
		}

		t.Run("builder refuses bad input", func(t *testing.T) {
			bads := []struct {
				env string
				lvl *hostv1.HieraLevel
			}{
				{"", &hostv1.HieraLevel{Name: "a"}},
				{"prod", nil},
				{"prod", &hostv1.HieraLevel{}},
				{"prod", &hostv1.HieraLevel{Name: "a", LookupOptions: map[string]string{"k": "deep"}}},
			}
			for i, b := range bads {
				if _, err := OverwriteBodyForHieraLevel(b.env, b.lvl, 0, false); !errors.Is(err, ErrOverwriteBodyInvalid) {
					t.Fatalf("case %d: got %v, want ErrOverwriteBodyInvalid", i, err)
				}
			}
		})

		t.Run("a non-integral index is refused on decode", func(t *testing.T) {
			body, _ := OverwriteBodyForHieraLevel("prod", &hostv1.HieraLevel{Name: "a"}, 1, true)
			stored := viaStore(t, body)
			stored["payload"].(map[string]any)["index"] = 1.5
			if _, _, _, err := OverwritePayloadHieraLevel(stored); !errors.Is(err, ErrOverwriteBodyInvalid) {
				t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
			}
		})
	})

	t.Run("hiera data key", func(t *testing.T) {
		cases := map[string]*hostv1.Json{
			"scalar":        {Value: mustStructT(t, map[string]any{"v": "text"})},
			"number":        {Value: mustStructT(t, map[string]any{"v": float64(8080)})},
			"list":          {Value: mustStructT(t, map[string]any{"v": []any{"a", "b"}})},
			"object valued": {Value: mustStructT(t, map[string]any{"host": "h", "port": float64(1), "nested": map[string]any{"k": true}})},
		}
		for name, in := range cases {
			t.Run(name, func(t *testing.T) {
				body, err := OverwriteBodyForHieraDataKey("prod", "nodes/web01.yaml", "app::port", in)
				if err != nil {
					t.Fatalf("OverwriteBodyForHieraDataKey: %v", err)
				}
				if _, has := body["status"]; has {
					t.Fatal("a builder must never write a status key")
				}
				want := OverwriteTarget{Environment: "prod", Resource: OverwriteResourceHieraDataKey, Name: "app::port", Path: "nodes/web01.yaml"}
				if got := mustTarget(t, viaStore(t, body)); got != want {
					t.Fatalf("target = %+v, want %+v", got, want)
				}
				out, err := OverwritePayloadHieraDataKey(viaStore(t, body))
				if err != nil {
					t.Fatalf("OverwritePayloadHieraDataKey: %v", err)
				}
				if !proto.Equal(in, out) {
					t.Fatalf("round trip changed the value:\n in  %v\n out %v", in, out)
				}
			})
		}

		t.Run("path participates in the target", func(t *testing.T) {
			a, _ := OverwriteBodyForHieraDataKey("prod", "common.yaml", "k", &hostv1.Json{})
			b, _ := OverwriteBodyForHieraDataKey("prod", "nodes/web01.yaml", "k", &hostv1.Json{})
			if mustTarget(t, a) == mustTarget(t, b) {
				t.Fatal("the same key in two files must be two different targets")
			}
		})

		t.Run("builder refuses missing identity", func(t *testing.T) {
			for _, c := range [][3]string{{"", "p", "k"}, {"e", "", "k"}, {"e", "p", ""}} {
				if _, err := OverwriteBodyForHieraDataKey(c[0], c[1], c[2], nil); !errors.Is(err, ErrOverwriteBodyInvalid) {
					t.Fatalf("%v: got %v, want ErrOverwriteBodyInvalid", c, err)
				}
			}
		})

		t.Run("payload of another resource kind is refused", func(t *testing.T) {
			body, _ := OverwriteBodyForSettings(&hostv1.EnvironmentSettings{Environment: "prod"})
			if _, err := OverwritePayloadHieraDataKey(viaStore(t, body)); !errors.Is(err, ErrOverwriteBodyInvalid) {
				t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
			}
			if _, _, _, err := OverwritePayloadHieraLevel(viaStore(t, body)); !errors.Is(err, ErrOverwriteBodyInvalid) {
				t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
			}
		})
	})
}

func mustStructT(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	st, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestParseOverwriteTargetRejects(t *testing.T) {
	good := func() map[string]any {
		return map[string]any{
			"target": map[string]any{"environment": "prod", "resource": OverwriteResourceSettings},
		}
	}

	if _, err := ParseOverwriteTarget(good()); err != nil {
		t.Fatalf("baseline body must parse: %v", err)
	}

	cases := map[string]func(m map[string]any){
		"absent target key":  func(m map[string]any) { delete(m, "target") },
		"non-object target":  func(m map[string]any) { m["target"] = "prod" },
		"resource not known": func(m map[string]any) { m["target"].(map[string]any)["resource"] = "hiera_datafile" },
		"empty resource":     func(m map[string]any) { m["target"].(map[string]any)["resource"] = "" },
		"missing resource":   func(m map[string]any) { delete(m["target"].(map[string]any), "resource") },
		"empty environment":  func(m map[string]any) { m["target"].(map[string]any)["environment"] = "" },
		"non-string field":   func(m map[string]any) { m["target"].(map[string]any)["name"] = 7.0 },
		"case-variant kind":  func(m map[string]any) { m["target"].(map[string]any)["resource"] = "Settings" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body := good()
			mutate(body)
			if _, err := ParseOverwriteTarget(body); !errors.Is(err, ErrOverwriteBodyInvalid) {
				t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
			}
		})
	}
}

// importSnapshotFixture is a two-branch snapshot exercising every field the
// proposal body must carry: findings (with line, kind, severity, excerpt), data
// files, settings and the per-branch flags. Branch names are deliberately out of
// order so the sorted-name join in the target is observable.
func importSnapshotFixture() *hostv1.ImportSnapshot {
	return &hostv1.ImportSnapshot{
		Url: "https://git.example.com/org/control-repo.git",
		Branches: []*hostv1.ImportBranchSnapshot{
			{
				Branch:         "staging",
				Commit:         "0123456789abcdef0123456789abcdef01234567",
				Importable:     true,
				WillOverwrite:  true,
				PuppetfileText: "mod 'puppetlabs/stdlib', '9.0.0'\n",
				HieraYaml:      "---\nversion: 5\n",
				DataFiles: []*hostv1.ImportDataFile{
					{Path: "common.yaml", Yaml: "---\nntp::server: pool.ntp.org\n"},
					{Path: "nodes/web01.yaml", Yaml: "---\nnote: \"caf\\u00e9 \u2603\"\n"},
				},
				Settings: &hostv1.EnvironmentSettings{Modulepath: strp("site:modules"), RichData: boolp(true)},
				Findings: []*hostv1.ImportFinding{
					{
						Branch: "staging", File: "Puppetfile", Line: 12, Kind: "puppetfile_forge_directive",
						Severity: hostv1.ImportFinding_WARNING, Excerpt: "forge 'https://forge.example.com'",
						Message: "a forge directive cannot be represented and was skipped",
					},
					{
						Branch: "staging", File: "hiera.yaml", Line: 0, Kind: "hiera_unparseable",
						Severity: hostv1.ImportFinding_ERROR, Excerpt: "{{{", Message: "file is absent from the snapshot",
					},
				},
			},
			{
				Branch:     "production",
				Commit:     "89abcdef0123456789abcdef0123456789abcdef",
				Importable: true,
			},
		},
	}
}

func TestOverwriteImportBodyRoundTrip(t *testing.T) {
	in := importSnapshotFixture()
	body, err := OverwriteBodyForImport(in)
	if err != nil {
		t.Fatalf("OverwriteBodyForImport: %v", err)
	}

	t.Run("target is fixed by construction", func(t *testing.T) {
		want := OverwriteTarget{
			Environment: "",
			Resource:    OverwriteResourceImport,
			Source:      "https://git.example.com/org/control-repo.git",
			Name:        "production,staging",
			Path:        "",
		}
		if got := mustTarget(t, viaStore(t, body)); got != want {
			t.Fatalf("target = %+v, want %+v", got, want)
		}
	})

	t.Run("snapshot round trips byte-equal through the store", func(t *testing.T) {
		out, err := OverwritePayloadImport(viaStore(t, body))
		if err != nil {
			t.Fatalf("OverwritePayloadImport: %v", err)
		}
		if !proto.Equal(in, out) {
			t.Fatalf("round trip changed the snapshot:\n in  %v\n out %v", in, out)
		}
		// The line number must survive as a number, not become a string.
		f := out.GetBranches()[0].GetFindings()[0]
		if f.GetLine() != 12 || f.GetKind() != "puppetfile_forge_directive" || f.GetSeverity() != hostv1.ImportFinding_WARNING || f.GetExcerpt() == "" {
			t.Fatalf("finding lost fields on the round trip: %v", f)
		}
	})

	t.Run("the builder does not mutate its input", func(t *testing.T) {
		if in.GetBranches()[0].GetBranch() != "staging" {
			t.Fatalf("builder reordered the caller's branches: %v", in.GetBranches()[0].GetBranch())
		}
	})

	t.Run("builder refuses bad input", func(t *testing.T) {
		bads := map[string]*hostv1.ImportSnapshot{
			"nil":         nil,
			"empty url":   {Branches: []*hostv1.ImportBranchSnapshot{{Branch: "production"}}},
			"no branches": {Url: "https://git.example.com/r.git"},
		}
		for name, s := range bads {
			t.Run(name, func(t *testing.T) {
				if _, err := OverwriteBodyForImport(s); !errors.Is(err, ErrOverwriteBodyInvalid) {
					t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
				}
			})
		}
	})

	t.Run("payload of another resource kind is refused", func(t *testing.T) {
		other, _ := OverwriteBodyForSettings(&hostv1.EnvironmentSettings{Environment: "prod"})
		_, err := OverwritePayloadImport(viaStore(t, other))
		if !errors.Is(err, ErrOverwriteBodyInvalid) {
			t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
		}
		if err != nil && !strings.Contains(err.Error(), "settings") {
			t.Fatalf("mismatch error should name the actual resource: %v", err)
		}
	})

	t.Run("no governance key at any depth", func(t *testing.T) {
		banned := map[string]bool{"status": true, "approved_scope": true, "decided_by": true, "decided_at": true, "reason": true}
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, child := range x {
					if banned[k] {
						t.Errorf("body carries governance key %q at %s", k, path)
					}
					walk(path+"."+k, child)
				}
			case []any:
				for i, child := range x {
					walk(fmt.Sprintf("%s[%d]", path, i), child)
				}
			}
		}
		walk("body", body)
	})
}

func TestParseOverwriteTargetImportRelaxationIsScoped(t *testing.T) {
	mk := func(resource, env, source string) map[string]any {
		return map[string]any{"target": map[string]any{"environment": env, "resource": resource, "source": source}}
	}

	t.Run("import with empty environment and a source parses", func(t *testing.T) {
		got, err := ParseOverwriteTarget(mk(OverwriteResourceImport, "", "https://git.example.com/r.git"))
		if err != nil {
			t.Fatalf("got %v, want success", err)
		}
		if got.Environment != "" || got.Resource != OverwriteResourceImport || got.Source != "https://git.example.com/r.git" {
			t.Fatalf("unexpected target %+v", got)
		}
	})

	t.Run("import with empty environment and empty source is refused", func(t *testing.T) {
		if _, err := ParseOverwriteTarget(mk(OverwriteResourceImport, "", "")); !errors.Is(err, ErrOverwriteBodyInvalid) {
			t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
		}
	})

	t.Run("each pre-existing kind still refuses an empty environment", func(t *testing.T) {
		kinds := []string{
			OverwriteResourceSettings,
			OverwriteResourcePuppetfileModule,
			OverwriteResourceHieraLevel,
			OverwriteResourceHieraDataKey,
			OverwriteResourceEnvironment,
		}
		for _, kind := range kinds {
			t.Run(kind, func(t *testing.T) {
				// A non-empty source must not rescue a non-import kind.
				if _, err := ParseOverwriteTarget(mk(kind, "", "https://git.example.com/r.git")); !errors.Is(err, ErrOverwriteBodyInvalid) {
					t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
				}
			})
		}
	})

	t.Run("only the exact literal reaches the relaxation", func(t *testing.T) {
		for _, kind := range []string{"imports", "Import", "import "} {
			t.Run(fmt.Sprintf("%q", kind), func(t *testing.T) {
				_, err := ParseOverwriteTarget(mk(kind, "", "https://git.example.com/r.git"))
				if !errors.Is(err, ErrOverwriteBodyInvalid) {
					t.Fatalf("got %v, want ErrOverwriteBodyInvalid", err)
				}
				if err != nil && !strings.Contains(err.Error(), "resource") {
					t.Fatalf("want an unknown-resource refusal, got %v", err)
				}
			})
		}
	})
}

// TestPuppetfileModuleTargetName pins the one source-aware rule that produces a
// puppetfile_module overwrite target's Name: a Forge module is folded to its
// canonical name, a Git module's bare name is never touched.
func TestPuppetfileModuleTargetName(t *testing.T) {
	forge := func(name string) *hostv1.PuppetfileModule {
		return &hostv1.PuppetfileModule{Name: name, Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}}
	}
	git := func(name string) *hostv1.PuppetfileModule {
		return &hostv1.PuppetfileModule{Name: name, Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/m.git"}}}
	}
	cases := []struct {
		name string
		m    *hostv1.PuppetfileModule
		want string
	}{
		{"forge slash form", forge("puppetlabs/stdlib"), "puppetlabs-stdlib"},
		{"forge hyphen form", forge("puppetlabs-stdlib"), "puppetlabs-stdlib"},
		{"forge mixed-case owner", forge("PuppetLabs/stdlib"), "puppetlabs-stdlib"},
		{"git bare name with capitals is untouched", git("My-Module"), "My-Module"},
		{"git bare hyphenated name is untouched", git("my-module"), "my-module"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PuppetfileModuleTargetName(tc.m); got != tc.want {
				t.Fatalf("PuppetfileModuleTargetName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOverwriteBodyForPuppetfileModuleTargetIsCanonical pins that the proposal
// target carries the canonical name while the frozen payload keeps the module
// exactly as the approver saw it.
func TestOverwriteBodyForPuppetfileModuleTargetIsCanonical(t *testing.T) {
	m := &hostv1.PuppetfileModule{
		Name:   "puppetlabs/stdlib",
		Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "9.4.1"}},
	}
	body, err := OverwriteBodyForPuppetfileModule("prod", m)
	if err != nil {
		t.Fatalf("OverwriteBodyForPuppetfileModule: %v", err)
	}
	stored := viaStore(t, body)
	if got := mustTarget(t, stored).Name; got != "puppetlabs-stdlib" {
		t.Fatalf("target name = %q, want the canonical puppetlabs-stdlib", got)
	}
	frozen, err := OverwritePayloadPuppetfileModule(stored)
	if err != nil {
		t.Fatalf("OverwritePayloadPuppetfileModule: %v", err)
	}
	if frozen.GetName() != "puppetlabs/stdlib" {
		t.Fatalf("frozen payload name = %q, want it exactly as supplied", frozen.GetName())
	}
}
