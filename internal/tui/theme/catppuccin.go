package theme

func catppuccin() *Theme {
	return &Theme{
		Name:       "catppuccin",
		Primary:    "14",    // ANSI Bright Cyan
		Secondary:  "13",    // ANSI Bright Magenta
		Text:       "252",   // light grey
		TextMuted:  "243",   // grey
		Background: "235",   // dark grey
		Error:      "196",   // red
		Warning:    "214",   // amber
		Success:    "70",    // green
		CodeBG:     "233",   // very dark
		CodeFG:     "187",   // warm beige
		CodeAccent: "215",   // orange
		Link:       "33",    // blue
		Blockquote: "242",   // dim grey
		Heading:    "14",    // cyan
		ListBullet: "14",    // cyan
		Border:     "239",   // grey
		OverlayBG:  "234",   // dark
		SelectedBG: "14",    // cyan
		SelectedFG: "232",   // nearly black
		DimBG:      "233",   // very dark
	}
}
