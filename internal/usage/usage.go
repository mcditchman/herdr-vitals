// Package usage turns a context size in tokens into what the sidebar shows:
// a share of the model's context window, drawn as a bar.
package usage

import (
	"fmt"
	"strings"
)

const (
	OneMillion  int64 = 1_000_000
	TwoHundredK int64 = 200_000
)

// legacyWindows are the model id prefixes that predate 1M as the default
// context window. Every current model (Opus 4.6+, Sonnet 4.6+, Haiku 5.5,
// Fable) has a 1M window, so anything not listed here falls to the default.
var legacyWindows = []string{
	"claude-3",
	"claude-haiku-4-5",
	"claude-sonnet-4-5",
	"claude-sonnet-4-2", // claude-sonnet-4-20250514
	"claude-opus-4-5",
	"claude-opus-4-1",
	"claude-opus-4-2", // claude-opus-4-20250514
}

// oneMillionSuffix is how Claude Code marks a model run with the 1M window
// opted into, e.g. "claude-sonnet-4-5[1m]".
const oneMillionSuffix = "[1m]"

// WindowFor reports the context window of model in tokens. Overrides are model
// id prefixes and the longest match wins; then the [1m] suffix, then the
// legacy list, then fallback.
func WindowFor(model string, overrides map[string]int64, fallback int64) int64 {
	base, suffix, _ := strings.Cut(model, "[")

	if window, ok := longestPrefix(base, overrides); ok {
		return window
	}

	if "["+suffix == oneMillionSuffix {
		return OneMillion
	}

	for _, prefix := range legacyWindows {
		if strings.HasPrefix(base, prefix) {
			return TwoHundredK
		}
	}

	return fallback
}

func longestPrefix(model string, overrides map[string]int64) (int64, bool) {
	var (
		best    int64
		bestLen = -1
	)

	for prefix, window := range overrides {
		if window <= 0 || !strings.HasPrefix(model, prefix) || len(prefix) <= bestLen {
			continue
		}

		best, bestLen = window, len(prefix)
	}

	return best, bestLen >= 0
}

// Percent is tokens as a whole-number share of window, rounded half up and
// clamped to 0..100.
func Percent(tokens, window int64) int {
	if window <= 0 || tokens <= 0 {
		return 0
	}

	pct := (tokens*100 + window/2) / window

	return int(min(pct, 100))
}

const (
	filled = "▰"
	empty  = "▱"
)

// Bar draws pct as a width-cell bar followed by the number, e.g.
// "▰▰▰▱▱▱▱▱ 41%". A width of zero or less draws the number alone.
func Bar(pct, width int) string {
	pct = max(0, min(pct, 100))
	label := fmt.Sprintf("%d%%", pct)

	if width <= 0 {
		return label
	}

	cells := (pct*width + 50) / 100

	return strings.Repeat(filled, cells) + strings.Repeat(empty, width-cells) + " " + label
}
