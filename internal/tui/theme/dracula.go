package theme

func dracula() *Theme {
	return &Theme{
		Name:       "dracula",
		Primary:    "141",   // purple
		Secondary:  "212",   // pink
		Text:       "255",   // white
		TextMuted:  "246",   // grey
		Background: "235",   // dark
		Error:      "196",   // red
		Warning:    "215",   // orange
		Success:    "83",    // green
		CodeBG:     "234",
		CodeFG:     "229",
		CodeAccent: "218",
		Link:       "117",   // light blue
		Blockquote: "245",
		Heading:    "141",
		ListBullet: "212",
		Border:     "237",
		OverlayBG:  "236",
		SelectedBG: "141",
		SelectedFG: "231",
		DimBG:      "233",
	}
}
