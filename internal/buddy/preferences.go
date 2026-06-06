package buddy

import "strings"

type OverlayPosition string

const (
	OverlayRightBottom OverlayPosition = "right-bottom"
	OverlayRightMiddle OverlayPosition = "right-middle"
	OverlayLeftBottom  OverlayPosition = "left-bottom"
)

type AnimationIntensity string

const (
	AnimationLow    AnimationIntensity = "low"
	AnimationMedium AnimationIntensity = "medium"
	AnimationHigh   AnimationIntensity = "high"
)

type BehaviorPack string

const (
	BehaviorCoding BehaviorPack = "coding"
	BehaviorReview BehaviorPack = "review"
	BehaviorDebug  BehaviorPack = "debug"
)

type OverlayMode string

const (
	OverlayOn  OverlayMode = "on"
	OverlayOff OverlayMode = "off"
)

type CompanionMode string

const (
	CompanionPractical CompanionMode = "practical"
	CompanionPlayful   CompanionMode = "playful"
)

type Preferences struct {
	Position  OverlayPosition    `json:"position"`
	Intensity AnimationIntensity `json:"intensity"`
	Behavior  BehaviorPack       `json:"behavior"`
	Overlay   OverlayMode        `json:"overlay"`
	Mode      CompanionMode      `json:"mode"`
}

func DefaultPreferences() Preferences {
	return Preferences{
		Position:  OverlayRightBottom,
		Intensity: AnimationMedium,
		Behavior:  BehaviorCoding,
		Overlay:   OverlayOn,
		Mode:      CompanionPractical,
	}
}

func NormalizePreferences(p Preferences) Preferences {
	if v, ok := ParsePosition(string(p.Position)); ok {
		p.Position = v
	} else {
		p.Position = OverlayRightBottom
	}
	if v, ok := ParseIntensity(string(p.Intensity)); ok {
		p.Intensity = v
	} else {
		p.Intensity = AnimationMedium
	}
	if v, ok := ParseBehaviorPack(string(p.Behavior)); ok {
		p.Behavior = v
	} else {
		p.Behavior = BehaviorCoding
	}
	if v, ok := ParseOverlayMode(string(p.Overlay)); ok {
		p.Overlay = v
	} else {
		p.Overlay = OverlayOn
	}
	if v, ok := ParseCompanionMode(string(p.Mode)); ok {
		p.Mode = v
	} else {
		p.Mode = CompanionPractical
	}
	return p
}

func ParsePosition(v string) (OverlayPosition, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case string(OverlayRightBottom):
		return OverlayRightBottom, true
	case string(OverlayRightMiddle):
		return OverlayRightMiddle, true
	case string(OverlayLeftBottom):
		return OverlayLeftBottom, true
	default:
		return "", false
	}
}

func ParseIntensity(v string) (AnimationIntensity, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case string(AnimationLow):
		return AnimationLow, true
	case string(AnimationMedium):
		return AnimationMedium, true
	case string(AnimationHigh):
		return AnimationHigh, true
	default:
		return "", false
	}
}

func ParseBehaviorPack(v string) (BehaviorPack, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case string(BehaviorCoding):
		return BehaviorCoding, true
	case string(BehaviorReview):
		return BehaviorReview, true
	case string(BehaviorDebug):
		return BehaviorDebug, true
	default:
		return "", false
	}
}

func ParseOverlayMode(v string) (OverlayMode, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case string(OverlayOn):
		return OverlayOn, true
	case string(OverlayOff):
		return OverlayOff, true
	default:
		return "", false
	}
}

func ParseCompanionMode(v string) (CompanionMode, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case string(CompanionPractical):
		return CompanionPractical, true
	case string(CompanionPlayful):
		return CompanionPlayful, true
	default:
		return "", false
	}
}
