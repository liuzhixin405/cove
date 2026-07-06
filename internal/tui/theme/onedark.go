package theme

func onedark() *Theme {
	return &Theme{
		Name:       "onedark",
		Primary:    "75",    // bright blue
		Secondary:  "204",   // red-ish pink
		Text:       "188",   // light grey
		TextMuted:  "243",   // grey
		Background: "236",
		Error:      "167",   // red
		Warning:    "215",   // orange
		Success:    "114",   // green
		CodeBG:     "235",
		CodeFG:     "188",
		CodeAccent: "180",
		Link:       "75",
		Blockquote: "243",
		Heading:    "75",
		ListBullet: "204",
		Border:     "240",
		OverlayBG:  "235",
		SelectedBG: "75",
		SelectedFG: "16",
		DimBG:      "234",
	}
}
