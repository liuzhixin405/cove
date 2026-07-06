package theme

func tokyonight() *Theme {
	return &Theme{
		Name:       "tokyonight",
		Primary:    "111",   // blue
		Secondary:  "212",   // pink
		Text:       "255",   // white-ish
		TextMuted:  "243",
		Background: "234",
		Error:      "203",
		Warning:    "215",
		Success:    "120",
		CodeBG:     "233",
		CodeFG:     "189",
		CodeAccent: "182",
		Link:       "111",
		Blockquote: "243",
		Heading:    "111",
		ListBullet: "212",
		Border:     "237",
		OverlayBG:  "235",
		SelectedBG: "111",
		SelectedFG: "16",
		DimBG:      "233",
	}
}
