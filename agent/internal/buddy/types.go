package buddy

// Rarity determines stat floors and visual flair.
type Rarity string

const (
	Common    Rarity = "common"
	Uncommon  Rarity = "uncommon"
	Rare      Rarity = "rare"
	Epic      Rarity = "epic"
	Legendary Rarity = "legendary"
)

var Rarities = []Rarity{Common, Uncommon, Rare, Epic, Legendary}

var RarityWeights = map[Rarity]int{
	Common:    60,
	Uncommon:  25,
	Rare:      10,
	Epic:      4,
	Legendary: 1,
}

var RarityStars = map[Rarity]string{
	Common:    "★",
	Uncommon:  "★★",
	Rare:      "★★★",
	Epic:      "★★★★",
	Legendary: "★★★★★",
}

var RarityFloor = map[Rarity]int{
	Common:    5,
	Uncommon:  15,
	Rare:      25,
	Epic:      35,
	Legendary: 50,
}

// Species is the creature type.
type Species string

const (
	Duck     Species = "duck"
	Goose    Species = "goose"
	Blob     Species = "blob"
	Cat      Species = "cat"
	Dragon   Species = "dragon"
	Octopus  Species = "octopus"
	Owl      Species = "owl"
	Penguin  Species = "penguin"
	Turtle   Species = "turtle"
	Snail    Species = "snail"
	Ghost    Species = "ghost"
	Axolotl  Species = "axolotl"
	Capybara Species = "capybara"
	Cactus   Species = "cactus"
	Robot    Species = "robot"
	Rabbit   Species = "rabbit"
	Mushroom Species = "mushroom"
	Chonk    Species = "chonk"
)

var AllSpecies = []Species{
	Duck, Goose, Blob, Cat, Dragon, Octopus, Owl, Penguin,
	Turtle, Snail, Ghost, Axolotl, Capybara, Cactus, Robot, Rabbit, Mushroom, Chonk,
}

// Eye style for the sprite.
type Eye string

var AllEyes = []Eye{"·", "✦", "×", "◉", "@", "°"}

// Hat decoration.
type Hat string

const (
	HatNone      Hat = "none"
	HatCrown     Hat = "crown"
	HatTophat    Hat = "tophat"
	HatPropeller Hat = "propeller"
	HatHalo      Hat = "halo"
	HatWizard    Hat = "wizard"
	HatBeanie    Hat = "beanie"
	HatTinyduck  Hat = "tinyduck"
)

var AllHats = []Hat{HatNone, HatCrown, HatTophat, HatPropeller, HatHalo, HatWizard, HatBeanie, HatTinyduck}

// StatName is a companion attribute.
type StatName string

const (
	StatDebugging StatName = "DEBUGGING"
	StatPatience  StatName = "PATIENCE"
	StatChaos     StatName = "CHAOS"
	StatWisdom    StatName = "WISDOM"
	StatSnark     StatName = "SNARK"
)

var AllStats = []StatName{StatDebugging, StatPatience, StatChaos, StatWisdom, StatSnark}

// Bones are deterministic traits derived from a user hash. Never stored.
type Bones struct {
	Rarity  Rarity
	Species Species
	Eye     Eye
	Hat     Hat
	Shiny   bool
	Stats   map[StatName]int
}

// Soul is the model-generated personality, stored in config after first hatch.
type Soul struct {
	Name        string `json:"name"`
	Personality string `json:"personality"`
}

// Companion is the complete buddy state (Bones + Soul).
type Companion struct {
	Bones
	Soul
	HatchedAt int64 `json:"hatched_at"`
}
