package usage

import "testing"

func TestWindowFor(t *testing.T) {
	overrides := map[string]int64{
		"claude-sonnet-5":     300_000,
		"claude-sonnet-5-5":   400_000,
		"claude-custom-exact": 123,
	}

	tests := []struct {
		name      string
		model     string
		overrides map[string]int64
		want      int64
	}{
		{"current opus is 1M", "claude-opus-5-5", nil, OneMillion},
		{"current haiku is 1M", "claude-haiku-5-5", nil, OneMillion},
		{"sonnet 4.6 is 1M", "claude-sonnet-4-6", nil, OneMillion},
		{"haiku 4.5 is 200k", "claude-haiku-4-5", nil, TwoHundredK},
		{"dated haiku 4.5 is 200k", "claude-haiku-4-5-20251001", nil, TwoHundredK},
		{"sonnet 4.5 is 200k", "claude-sonnet-4-5", nil, TwoHundredK},
		{"dated sonnet 4 is 200k", "claude-sonnet-4-20250514", nil, TwoHundredK},
		{"dated opus 4 is 200k", "claude-opus-4-20250514", nil, TwoHundredK},
		{"opus 4.1 is 200k", "claude-opus-4-1", nil, TwoHundredK},
		{"opus 4.5 is 200k", "claude-opus-4-5", nil, TwoHundredK},
		{"claude 3 family is 200k", "claude-3-7-sonnet-latest", nil, TwoHundredK},
		{"[1m] suffix forces 1M on a 200k model", "claude-sonnet-4-5[1m]", nil, OneMillion},
		{"suffix is ignored for lookup", "claude-haiku-4-5[fast]", nil, TwoHundredK},
		{"unknown model falls back to 1M", "some-future-model", nil, OneMillion},
		{"empty model falls back to 1M", "", nil, OneMillion},
		{"longest override prefix wins", "claude-sonnet-5-5", overrides, 400_000},
		{"shorter override still matches", "claude-sonnet-5", overrides, 300_000},
		{"override beats builtin", "claude-custom-exact", overrides, 123},
		{"no override match uses builtin", "claude-haiku-4-5", overrides, TwoHundredK},
		{"non-positive override is ignored", "claude-opus-5-5", map[string]int64{"claude-opus": 0}, OneMillion},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WindowFor(tt.model, tt.overrides, OneMillion); got != tt.want {
				t.Fatalf("WindowFor(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestWindowForCustomDefault(t *testing.T) {
	if got := WindowFor("mystery", nil, 500_000); got != 500_000 {
		t.Fatalf("got %d, want 500000", got)
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		name   string
		tokens int64
		window int64
		want   int
	}{
		{"zero", 0, 1000, 0},
		{"rounds down", 414, 1000, 41},
		{"rounds half up", 415, 1000, 42},
		{"exact full", 1000, 1000, 100},
		{"over full clamps", 1500, 1000, 100},
		{"negative clamps", -5, 1000, 0},
		{"zero window is 0, not a divide by zero", 5, 0, 0},
		{"large numbers do not overflow", 999_999, 1_000_000, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Percent(tt.tokens, tt.window); got != tt.want {
				t.Fatalf("Percent(%d, %d) = %d, want %d", tt.tokens, tt.window, got, tt.want)
			}
		})
	}
}

func TestBar(t *testing.T) {
	tests := []struct {
		pct   int
		width int
		want  string
	}{
		{0, 8, "▱▱▱▱▱▱▱▱ 0%"},
		{41, 8, "▰▰▰▱▱▱▱▱ 41%"},
		{50, 8, "▰▰▰▰▱▱▱▱ 50%"},
		{6, 8, "▱▱▱▱▱▱▱▱ 6%"},
		{7, 8, "▰▱▱▱▱▱▱▱ 7%"},
		{99, 8, "▰▰▰▰▰▰▰▰ 99%"},
		{100, 8, "▰▰▰▰▰▰▰▰ 100%"},
		{150, 8, "▰▰▰▰▰▰▰▰ 100%"},
		{-1, 8, "▱▱▱▱▱▱▱▱ 0%"},
		{41, 4, "▰▰▱▱ 41%"}, // 1.64 cells rounds to 2
		{37, 4, "▰▱▱▱ 37%"}, // 1.48 cells rounds to 1
		{41, 0, "41%"},
		{41, -3, "41%"},
	}

	for _, tt := range tests {
		if got := Bar(tt.pct, tt.width); got != tt.want {
			t.Errorf("Bar(%d, %d) = %q, want %q", tt.pct, tt.width, got, tt.want)
		}
	}
}
