package lint

import (
	"fmt"

	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

// isEmoji reports characters that render as emoji by default. Text symbols the
// design uses as status glyphs (check, cross, arrows) are not in this set.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF:
		return true
	case r == 0xFE0F || r == 0x200D:
		return true
	case r == 0x2B50 || r == 0x2B55 || r == 0x2705 || r == 0x274C || r == 0x274E || r == 0x2728:
		return true
	case r >= 0x231A && r <= 0x231B, r >= 0x23E9 && r <= 0x23F3, r >= 0x25FD && r <= 0x25FE:
		return true
	case r >= 0x2614 && r <= 0x2615, r >= 0x2648 && r <= 0x2653, r == 0x267F, r == 0x2693, r == 0x26A1,
		r >= 0x26AA && r <= 0x26AB, r >= 0x26BD && r <= 0x26BE, r >= 0x26C4 && r <= 0x26C5, r == 0x26CE,
		r == 0x26D4, r == 0x26EA, r >= 0x26F2 && r <= 0x26F3, r == 0x26F5, r == 0x26FA, r == 0x26FD:
		return true
	case r == 0x270A || r == 0x270B || r >= 0x2753 && r <= 0x2755, r == 0x2757, r >= 0x2795 && r <= 0x2797,
		r == 0x27B0, r == 0x27BF:
		return true
	}
	return false
}

func lintEmoji(path, text string) []manifest.Finding {
	code, _ := Scan(path, text)
	var out []manifest.Finding
	line := 1
	for _, r := range code {
		if r == '\n' {
			line++
		} else if isEmoji(r) {
			out = append(out, manifest.Finding{
				Code:    "ui_lint_emoji",
				Path:    fmt.Sprintf("%s:%d", path, line),
				Message: fmt.Sprintf("emoji U+%04X", r),
				Fix:     "Remove the emoji. Status is a text glyph plus a label (for example a check mark and the word corrected).",
			})
		}
	}
	return out
}
