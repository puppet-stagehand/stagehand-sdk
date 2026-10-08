// Package harness holds the real-tool evidence harness: containers that run
// r10k, g10k and Bolt for real, and the recorded fixtures those runs produce.
//
// This file is untagged, so the default go test ./... compiles it and (once the
// validator lands) checks the committed fixtures offline with no Docker. The
// container-driving tests live in harness_test.go behind the harness build tag
// and run only through harness/run.sh. See docs/harness.md for the overview.
package harness

// Fixture is one recorded real-tool observation, stored as
// harness/fixtures/<tool>-<tool_version>/<scenario>.json. Both the tagged
// driver and the untagged validator use this type.
//
// Fixtures are evidence, so every field is plain data: nothing here may hold a
// credential, a private key, an approver token or the operator's registry host.
type Fixture struct {
	Tool               string   `json:"tool"`
	ToolVersion        string   `json:"tool_version"`
	Scenario           string   `json:"scenario"`
	RecordedOnPlatform string   `json:"recorded_on_platform"`
	Argv               []string `json:"argv"`
	RunAs              string   `json:"run_as"`
	ExitCode           int      `json:"exit_code"`
	Stdout             string   `json:"stdout"`
	Stderr             string   `json:"stderr"`

	// ExpectLines are substrings that must appear in the normalised output in
	// compare mode. The comparison is a subset check so harmless tool chatter
	// does not flake it; the full normalised output is still stored above.
	ExpectLines []string `json:"expect_lines,omitempty"`

	// MarkerKeys is the sorted key set of the deploy marker the tool wrote.
	MarkerKeys []string `json:"marker_keys,omitempty"`

	// MarkerBefore and MarkerAfter hold the parsed marker, with timestamps and
	// commit SHAs normalised.
	MarkerBefore map[string]any `json:"marker_before,omitempty"`
	MarkerAfter  map[string]any `json:"marker_after,omitempty"`

	// Tree hashes prove read-only behaviour: the same live tree hashed before
	// and after, with and without the deploy markers.
	TreeHashBefore            string `json:"tree_hash_before,omitempty"`
	TreeHashAfter             string `json:"tree_hash_after,omitempty"`
	TreeHashExclMarkersBefore string `json:"tree_hash_excl_markers_before,omitempty"`
	TreeHashExclMarkersAfter  string `json:"tree_hash_excl_markers_after,omitempty"`

	FilesChanged []string `json:"files_changed,omitempty"`

	// Extra carries scenario-specific evidence such as an HTTP status code or
	// file ownership.
	Extra map[string]any `json:"extra,omitempty"`

	Notes string `json:"notes,omitempty"`
}
