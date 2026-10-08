package lint

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

var reTerm = regexp.MustCompile(`(?i)\b(converged|no-op)\b`)

func lintTerm(path, text string) []manifest.Finding {
	code, _ := Scan(path, text)
	var out []manifest.Finding
	for ln, l := range strings.Split(code, "\n") {
		for _, m := range reTerm.FindAllString(l, -1) {
			use := `"corrected"`
			if strings.EqualFold(m, "no-op") {
				use = `"dry run"`
			}
			out = append(out, manifest.Finding{
				Code: "ui_lint_term", Path: fmt.Sprintf("%s:%d", path, ln+1),
				Message: fmt.Sprintf("the word %q is not used in this product", strings.ToLower(m)),
				Fix:     "Say " + use + " instead (terminology: corrected, not converged; dry run, not no-op).",
			})
		}
	}
	return out
}
