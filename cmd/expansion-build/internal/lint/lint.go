package lint

import (
	"sort"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

// Source is one file to lint; Path is slash-separated and relative to the source root.
type Source struct{ Path, Text string }

// Run applies every lint to every source and returns findings sorted by Path.
func Run(src []Source) []manifest.Finding {
	var out []manifest.Finding
	for _, s := range src {
		out = append(out, lintColour(s.Path, s.Text)...)
		out = append(out, lintEmoji(s.Path, s.Text)...)
		out = append(out, lintFont(s.Path, s.Text)...)
		out = append(out, lintTerm(s.Path, s.Text)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
