package buddy

import (
	"context"
	"fmt"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
)

// HatchPrompt generates a prompt for the AI to name and give personality to the buddy.
func HatchPrompt(bones Bones) string {
	return fmt.Sprintf(`A new coding companion has appeared! It's a %s %s with %s eyes%s.

Its stats are:
- DEBUGGING: %d
- PATIENCE: %d  
- CHAOS: %d
- WISDOM: %d
- SNARK: %d

Give this companion:
1. A short, cute name (1-2 words, no more than 12 characters)
2. A brief personality description (one sentence, under 60 characters)

Reply in EXACTLY this format:
NAME: <name>
PERSONALITY: <description>`,
		bones.Rarity, bones.Species, bones.Eye,
		func() string {
			if bones.Shiny {
				return " (✨ SHINY!)"
			}
			return ""
		}(),
		bones.Stats[StatDebugging],
		bones.Stats[StatPatience],
		bones.Stats[StatChaos],
		bones.Stats[StatWisdom],
		bones.Stats[StatSnark],
	)
}

// ParseHatchResponse extracts name and personality from the AI response.
func ParseHatchResponse(response string) Soul {
	soul := Soul{
		Name:        "Buddy",
		Personality: "A friendly coding companion",
	}

	lines := strings.Split(response, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), "NAME:") {
			name := strings.TrimSpace(line[5:])
			if len(name) > 0 && len(name) <= 20 {
				soul.Name = name
			}
		}
		if strings.HasPrefix(strings.ToUpper(line), "PERSONALITY:") {
			p := strings.TrimSpace(line[12:])
			if len(p) > 0 && len(p) <= 100 {
				soul.Personality = p
			}
		}
	}
	return soul
}

// Hatch creates a new companion by asking the AI to generate a soul.
func Hatch(ctx context.Context, provider api.Provider, model string, userID string) (*Companion, error) {
	bones := Roll(userID)
	prompt := HatchPrompt(bones)

	resp, err := provider.Chat(ctx, api.ChatRequest{
		Model:      model,
		Messages:   []api.Message{{Role: "user", Content: prompt}},
		SystemBase: "You are naming a virtual pet companion for a developer. Be creative and fun. Keep responses concise.",
		MaxTokens:  200,
	})
	if err != nil {
		// Fallback: generate a simple name deterministically
		names := []string{"Pixel", "Byte", "Spark", "Boop", "Chip", "Glitch", "Fizz", "Blip"}
		rng := mulberry32(hashString(userID + "name"))
		soul := Soul{
			Name:        names[int(rng()*float64(len(names)))],
			Personality: fmt.Sprintf("A %s %s who loves code", bones.Rarity, bones.Species),
		}
		if err := SaveCompanion(soul); err != nil {
			return nil, err
		}
		return &Companion{Bones: bones, Soul: soul, HatchedAt: 0}, nil
	}

	soul := ParseHatchResponse(resp.Content)
	if err := SaveCompanion(soul); err != nil {
		return nil, err
	}

	return &Companion{
		Bones:     bones,
		Soul:      soul,
		HatchedAt: 0,
	}, nil
}
