package code

import (
	"errors"
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
