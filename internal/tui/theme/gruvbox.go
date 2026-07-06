package theme

func gruvbox() *Theme {
	return &Theme{
		Name:       "gruvbox",
		Primary:    "214",   // bright orange
		Secondary:  "142",   // olive green
		Text:       "223",   // light cream
		TextMuted:  "243",   // grey
		Background: "237",   // dark
		Error:      "167",   // red
		Warning:    "208",   // orange
		Success:    "142",   // green
		CodeBG:     "236",
		CodeFG:     "229",
		CodeAccent: "215",
		Link:       "109",   // blue-grey
		Blockquote: "245",
		Heading:    "214",
		ListBullet: "142",
		Border:     "239",
		OverlayBG:  "236",
		SelectedBG: "214",
		SelectedFG: "235",
		DimBG:      "235",
	}
}
