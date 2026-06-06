package buddy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const salt = "friend-2026-401"

// mulberry32 is a tiny seeded PRNG good enough for deterministic buddy generation.
func mulberry32(seed uint32) func() float64 {
	a := seed
	return func() float64 {
		a += 0x6D2B79F5
		t := a
		t = (t ^ (t >> 15)) * (1 | a)
		t = (t + (t^(t>>7))*(61|t)) ^ t
		return float64((t^(t>>14))>>0) / 4294967296.0
	}
}

// hashString produces a uint32 hash (FNV-1a).
func hashString(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func pick[T any](rng func() float64, arr []T) T {
	return arr[int(rng()*float64(len(arr)))]
}

func rollRarity(rng func() float64) Rarity {
	total := 0
	for _, w := range RarityWeights {
		total += w
	}
	roll := rng() * float64(total)
	for _, r := range Rarities {
		roll -= float64(RarityWeights[r])
		if roll < 0 {
			return r
		}
	}
	return Common
}

func rollStats(rng func() float64, rarity Rarity) map[StatName]int {
	floor := RarityFloor[rarity]
	peak := pick(rng, AllStats)
	dump := pick(rng, AllStats)
	for dump == peak {
		dump = pick(rng, AllStats)
	}

	stats := make(map[StatName]int, len(AllStats))
	for _, name := range AllStats {
		switch name {
		case peak:
			v := floor + 50 + int(rng()*30)
			if v > 100 {
				v = 100
			}
			stats[name] = v
		case dump:
			v := floor - 10 + int(rng()*15)
			if v < 1 {
				v = 1
			}
			stats[name] = v
		default:
			stats[name] = floor + int(rng()*40)
		}
	}
	return stats
}

// Roll generates deterministic bones from a user ID.
func Roll(userID string) Bones {
	key := userID + salt
	rng := mulberry32(hashString(key))

	rarity := rollRarity(rng)
	hat := HatNone
	if rarity != Common {
		hat = pick(rng, AllHats)
	}

	return Bones{
		Rarity:  rarity,
		Species: pick(rng, AllSpecies),
		Eye:     pick(rng, AllEyes),
		Hat:     hat,
		Shiny:   rng() < 0.01,
		Stats:   rollStats(rng, rarity),
	}
}

// StoredCompanion is what's saved to disk.
type StoredCompanion struct {
	Soul
	Preferences Preferences `json:"preferences"`
	HatchedAt   int64       `json:"hatched_at"`
}

// configPath returns the buddy config file path.
func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cove", "buddy.json")
}

// LoadCompanion loads the stored companion and regenerates bones from userID.
func LoadCompanion(userID string) *Companion {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil
	}
	var stored StoredCompanion
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil
	}
	if stored.Name == "" {
		return nil
	}
	bones := Roll(userID)
	prefs := NormalizePreferences(stored.Preferences)
	if prefs == (Preferences{}) {
		prefs = DefaultPreferences()
	}
	return &Companion{
		Bones:       bones,
		Soul:        stored.Soul,
		Preferences: prefs,
		HatchedAt:   stored.HatchedAt,
	}
}

// SaveCompanion stores the soul (name + personality) to disk.
func SaveCompanion(soul Soul) error {
	prefs := DefaultPreferences()
	if data, err := os.ReadFile(configPath()); err == nil {
		var prev StoredCompanion
		if json.Unmarshal(data, &prev) == nil {
			prefs = NormalizePreferences(prev.Preferences)
		}
	}
	stored := StoredCompanion{
		Soul:        soul,
		Preferences: prefs,
		HatchedAt:   time.Now().Unix(),
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(configPath()), 0700)
	return os.WriteFile(configPath(), data, 0644)
}

// UpdatePreferences updates only buddy style preferences in the saved companion.
func UpdatePreferences(prefs Preferences) error {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return err
	}
	var stored StoredCompanion
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	if stored.Name == "" {
		return os.ErrNotExist
	}
	stored.Preferences = NormalizePreferences(prefs)
	out, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(configPath()), 0700)
	return os.WriteFile(configPath(), out, 0644)
}

// GetUserID returns a stable identifier for buddy generation.
func GetUserID() string {
	home, _ := os.UserHomeDir()
	// Use machine-id or hostname as fallback
	hostname, _ := os.Hostname()
	if hostname != "" {
		return hostname + "-" + filepath.Base(home)
	}
	return "anon"
}
