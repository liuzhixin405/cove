package buddy

import "strings"

// Each species has 3 frames of ASCII art (5 lines tall, ~12 wide).
// {E} is replaced with the companion's eye character at render time.
var bodies = map[Species][][]string{
	Duck: {
		{"            ", "    __      ", "  <({E} )___  ", "   (  ._>   ", "    `--´    "},
		{"            ", "    __      ", "  <({E} )___  ", "   (  ._>   ", "    `--´~   "},
		{"            ", "    __      ", "  <({E} )___  ", "   (  .__>  ", "    `--´    "},
	},
	Goose: {
		{"            ", "     ({E}>    ", "     ||     ", "   _(__)_   ", "    ^^^^    "},
		{"            ", "    ({E}>     ", "     ||     ", "   _(__)_   ", "    ^^^^    "},
		{"            ", "     ({E}>>   ", "     ||     ", "   _(__)_   ", "    ^^^^    "},
	},
	Blob: {
		{"            ", "   .----.   ", "  ( {E}  {E} )  ", "  (      )  ", "   `----´   "},
		{"            ", "  .------.  ", " (  {E}  {E}  ) ", " (        ) ", "  `------´  "},
		{"            ", "    .--.    ", "   ({E}  {E})   ", "   (    )   ", "    `--´    "},
	},
	Cat: {
		{"            ", "   /\\_/\\    ", "  ( {E}   {E})  ", "  (  ω  )   ", "  (\")_(\")   "},
		{"            ", "   /\\_/\\    ", "  ( {E}   {E})  ", "  (  ω  )   ", "  (\")_(\")~  "},
		{"            ", "   /\\-/\\    ", "  ( {E}   {E})  ", "  (  ω  )   ", "  (\")_(\")   "},
	},
	Dragon: {
		{"            ", "  /^\\  /^\\  ", " <  {E}  {E}  > ", " (   ~~   ) ", "  `-vvvv-´  "},
		{"            ", "  /^\\  /^\\  ", " <  {E}  {E}  > ", " (        ) ", "  `-vvvv-´  "},
		{"   ~    ~   ", "  /^\\  /^\\  ", " <  {E}  {E}  > ", " (   ~~   ) ", "  `-vvvv-´  "},
	},
	Octopus: {
		{"            ", "   .----.   ", "  ( {E}  {E} )  ", "  (______)  ", "  /\\/\\/\\/\\  "},
		{"            ", "   .----.   ", "  ( {E}  {E} )  ", "  (______)  ", "  \\/\\/\\/\\/  "},
		{"     o      ", "   .----.   ", "  ( {E}  {E} )  ", "  (______)  ", "  /\\/\\/\\/\\  "},
	},
	Owl: {
		{"            ", "   /\\  /\\   ", "  (({E})({E}))  ", "  (  ><  )  ", "   `----´   "},
		{"            ", "   /\\  /\\   ", "  (({E})({E}))  ", "  (  ><  )  ", "   .----.   "},
		{"            ", "   /\\  /\\   ", "  (({E})(-))  ", "  (  ><  )  ", "   `----´   "},
	},
	Penguin: {
		{"            ", "  .---.     ", "  ({E}>{E})     ", " /(   )\\    ", "  `---´     "},
		{"            ", "  .---.     ", "  ({E}>{E})     ", " |(   )|    ", "  `---´     "},
		{"  .---.     ", "  ({E}>{E})     ", " /(   )\\    ", "  `---´     ", "   ~ ~      "},
	},
	Turtle: {
		{"            ", "   _,--._   ", "  ( {E}  {E} )  ", " /[______]\\ ", "  ``    ``  "},
		{"            ", "   _,--._   ", "  ( {E}  {E} )  ", " /[______]\\ ", "   ``  ``   "},
		{"            ", "   _,--._   ", "  ( {E}  {E} )  ", " /[======]\\ ", "  ``    ``  "},
	},
	Snail: {
		{"            ", " {E}    .--.  ", "  \\  ( @ )  ", "   \\_`--´   ", "  ~~~~~~~   "},
		{"            ", "  {E}   .--.  ", "  |  ( @ )  ", "   \\_`--´   ", "  ~~~~~~~   "},
		{"            ", " {E}    .--.  ", "  \\  ( @  ) ", "   \\_`--´   ", "   ~~~~~~   "},
	},
	Ghost: {
		{"            ", "   .----.   ", "  / {E}  {E} \\  ", "  |      |  ", "  ~`~``~`~  "},
		{"            ", "   .----.   ", "  / {E}  {E} \\  ", "  |      |  ", "  `~`~~`~`  "},
		{"    ~  ~    ", "   .----.   ", "  / {E}  {E} \\  ", "  |      |  ", "  ~~`~~`~~  "},
	},
	Axolotl: {
		{"            ", "}~(______)~{", "}~({E} .. {E})~{", "  ( .--. )  ", "  (_/  \\_)  "},
		{"            ", "~}(______){~", "~}({E} .. {E}){~", "  ( .--. )  ", "  (_/  \\_)  "},
		{"            ", "}~(______)~{", "}~({E} .. {E})~{", "  (  --  )  ", "  ~_/  \\_~  "},
	},
	Capybara: {
		{"            ", "  n______n  ", " ( {E}    {E} ) ", " (   oo   ) ", "  `------´  "},
		{"            ", "  n______n  ", " ( {E}    {E} ) ", " (   Oo   ) ", "  `------´  "},
		{"    ~  ~    ", "  u______n  ", " ( {E}    {E} ) ", " (   oo   ) ", "  `------´  "},
	},
	Cactus: {
		{"            ", " n  ____  n ", " | |{E}  {E}| | ", " |_|    |_| ", "   |    |   "},
		{"            ", "    ____    ", " n |{E}  {E}| n ", " |_|    |_| ", "   |    |   "},
		{" n        n ", " |  ____  | ", " | |{E}  {E}| | ", " |_|    |_| ", "   |    |   "},
	},
	Robot: {
		{"            ", "   .[||].   ", "  [ {E}  {E} ]  ", "  [ ==== ]  ", "  `------´  "},
		{"            ", "   .[||].   ", "  [ {E}  {E} ]  ", "  [ -==- ]  ", "  `------´  "},
		{"     *      ", "   .[||].   ", "  [ {E}  {E} ]  ", "  [ ==== ]  ", "  `------´  "},
	},
	Rabbit: {
		{"            ", "   (\\__/)   ", "  ( {E}  {E} )  ", " =(  ..  )= ", "  (\")__(\"  "},
		{"            ", "   (|__/)   ", "  ( {E}  {E} )  ", " =(  ..  )= ", "  (\")__(\")  "},
		{"            ", "   (\\__/)   ", "  ( {E}  {E} )  ", " =( .  . )= ", "  (\")__(\")  "},
	},
	Mushroom: {
		{"            ", " .-o-OO-o-. ", "(__________)", "   |{E}  {E}|   ", "   |____|   "},
		{"            ", " .-O-oo-O-. ", "(__________)", "   |{E}  {E}|   ", "   |____|   "},
		{"   . o  .   ", " .-o-OO-o-. ", "(__________)", "   |{E}  {E}|   ", "   |____|   "},
	},
	Chonk: {
		{"            ", "  /\\    /\\  ", " ( {E}    {E} ) ", " (   ..   ) ", "  `------´  "},
		{"            ", "  /\\    /|  ", " ( {E}    {E} ) ", " (   ..   ) ", "  `------´  "},
		{"            ", "  /\\    /\\  ", " ( {E}    {E} ) ", " (   ..   ) ", "  `------´~ "},
	},
}

var hatLines = map[Hat]string{
	HatNone:      "",
	HatCrown:     "   \\^^^/    ",
	HatTophat:    "   [___]    ",
	HatPropeller: "    -+-     ",
	HatHalo:      "   (   )    ",
	HatWizard:    "    /^\\     ",
	HatBeanie:    "   (___)    ",
	HatTinyduck:  "    ,>      ",
}

// RenderSprite returns the ASCII lines for a given frame.
func RenderSprite(bones Bones, frame int) []string {
	frames := bodies[bones.Species]
	if len(frames) == 0 {
		return []string{"  ???  "}
	}
	f := frames[frame%len(frames)]
	lines := make([]string, len(f))
	for i, line := range f {
		lines[i] = strings.ReplaceAll(line, "{E}", string(bones.Eye))
	}

	// Apply hat on line 0 if blank
	if bones.Hat != HatNone && strings.TrimSpace(lines[0]) == "" {
		if h, ok := hatLines[bones.Hat]; ok && h != "" {
			lines[0] = h
		}
	}
	return lines
}

// RenderFace returns a compact inline face representation.
func RenderFace(bones Bones) string {
	e := string(bones.Eye)
	switch bones.Species {
	case Duck, Goose:
		return "(" + e + ">"
	case Blob:
		return "(" + e + e + ")"
	case Cat:
		return "=" + e + "ω" + e + "="
	case Dragon:
		return "<" + e + "~" + e + ">"
	case Octopus:
		return "~(" + e + e + ")~"
	case Owl:
		return "(" + e + ")(" + e + ")"
	case Penguin:
		return "(" + e + ">)"
	case Turtle:
		return "[" + e + "_" + e + "]"
	case Snail:
		return e + "(@)"
	case Ghost:
		return "/" + e + e + "\\"
	case Axolotl:
		return "}" + e + "." + e + "{"
	case Capybara:
		return "(" + e + "oo" + e + ")"
	case Cactus:
		return "|" + e + "  " + e + "|"
	case Robot:
		return "[" + e + e + "]"
	case Rabbit:
		return "(" + e + ".." + e + ")"
	case Mushroom:
		return "|" + e + "  " + e + "|"
	case Chonk:
		return "(" + e + "." + e + ")"
	default:
		return "(" + e + e + ")"
	}
}

// FrameCount returns how many animation frames a species has.
func FrameCount(species Species) int {
	if f, ok := bodies[species]; ok {
		return len(f)
	}
	return 1
}
