package buddy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Display manages the buddy's visual state in the terminal.
type Display struct {
	mu        sync.Mutex
	companion *Companion
	reactor   *Reactor
	frame     int
	tick      int
	prefs     Preferences
	petUntil  time.Time
	Mood      *MoodTracker
	XP        *XPTracker
	PetCombo  *PetCombo
	Fortune   *DailyFortune

	// Proactive idle chatter
	lastActivity time.Time
	idleChatCh   chan string
	stopIdle     chan struct{}

	// Optional AI-powered chat for proactive messages
	ChatBackend *BuddyChat
}

// Idle animation sequence: frame indices.
// 0 = rest, 1 = fidget, 2 = alternate fidget, -1 = blink on frame 0
var idleSequence = []int{0, 0, 0, 0, 1, 0, 0, 0, -1, 0, 0, 2, 0, 0, 0}

// NewDisplay creates a buddy display instance.
func NewDisplay(c *Companion) *Display {
	prefs := NormalizePreferences(c.Preferences)
	d := &Display{
		companion:    c,
		reactor:      NewReactor(c),
		prefs:        prefs,
		Mood:         NewMoodTracker(),
		XP:           NewXPTracker(0),
		PetCombo:     NewPetCombo(),
		Fortune:      NewDailyFortune(GetUserID()),
		lastActivity: time.Now(),
		idleChatCh:   make(chan string, 1),
		stopIdle:     make(chan struct{}),
	}
	d.reactor.SetBehaviorPack(prefs.Behavior)
	d.reactor.SetCompanionMode(prefs.Mode)
	go d.idleChatLoop()
	return d
}

// Reactor returns the underlying reactor for triggering events.
func (d *Display) Reactor() *Reactor {
	return d.reactor
}

// Name returns the companion's display name.
func (d *Display) Name() string {
	return d.companion.Name
}

// Preferences returns a snapshot of the current buddy rendering preferences.
func (d *Display) Preferences() Preferences {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.prefs
}

// SetPreferences applies rendering/reaction preferences at runtime.
func (d *Display) SetPreferences(p Preferences) {
	d.mu.Lock()
	d.prefs = NormalizePreferences(p)
	d.companion.Preferences = d.prefs
	d.mu.Unlock()
	d.reactor.SetBehaviorPack(d.prefs.Behavior)
	d.reactor.SetCompanionMode(d.prefs.Mode)
}

// ReactWithMood triggers an event and updates mood/XP. Returns quip.
func (d *Display) ReactWithMood(event Event, detail string) *Quip {
	prefs := d.Preferences()
	d.Mood.OnEvent(event)
	leveled := false
	switch event {
	case EventToolSuccess:
		leveled = d.XP.Add(2)
	case EventToolError:
		d.XP.Add(1)
	case EventTurn:
		leveled = d.XP.Add(1)
	}
	if leveled {
		expires := 10 * time.Second
		switch prefs.Intensity {
		case AnimationLow:
			expires = 12 * time.Second
		case AnimationHigh:
			expires = 7 * time.Second
		}
		return &Quip{
			Text:    fmt.Sprintf("🎉 升级了！Lv.%d「%s」", d.XP.Level, d.XP.Title()),
			Expires: time.Now().Add(expires),
		}
	}
	q := d.reactor.React(event, detail)
	if q != nil {
		switch prefs.Intensity {
		case AnimationLow:
			q.Expires = time.Now().Add(10 * time.Second)
		case AnimationHigh:
			q.Expires = time.Now().Add(6 * time.Second)
		}
	}
	return q
}

// Tick advances the animation by one frame (call every ~500ms).
func (d *Display) Tick() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tick++
}

// Pet triggers the petting animation. Returns quip with possible combo bonus.
func (d *Display) Pet() *Quip {
	d.mu.Lock()
	d.petUntil = time.Now().Add(2500 * time.Millisecond)
	d.mu.Unlock()

	d.Mood.OnEvent(EventPet)
	d.XP.Add(1)

	// Check combo
	combo, special := d.PetCombo.Hit()
	if special != "" {
		return &Quip{
			Text:    fmt.Sprintf("[x%d] %s", combo, special),
			Expires: time.Now().Add(8 * time.Second),
		}
	}

	return d.reactor.React(EventPet, "")
}

// Render returns the current visual representation of the buddy.
// Includes sprite + optional speech bubble + optional pet hearts.
func (d *Display) Render() string {
	d.mu.Lock()
	isPetting := time.Now().Before(d.petUntil)
	prefs := d.prefs
	d.mu.Unlock()

	// Determine frame
	seqIdx := int(time.Now().UnixMilli()/frameStepMs(prefs.Intensity)) % len(idleSequence)
	frameIdx := idleSequence[seqIdx]
	if frameIdx < 0 {
		frameIdx = 0 // blink uses frame 0 with different eye (simplified here)
	}

	// Render sprite
	lines := RenderSprite(d.companion.Bones, frameIdx)

	var sb strings.Builder

	// Pet hearts above sprite
	if isPetting {
		elapsed := time.Since(d.petUntil.Add(-2500 * time.Millisecond))
		heartFrame := int(elapsed.Milliseconds()/heartStepMs(prefs.Intensity)) % len(PetHearts)
		sb.WriteString(PetHearts[heartFrame])
		sb.WriteString("\n")
	}

	// Sprite body
	for _, line := range lines {
		sb.WriteString(line)
		sb.WriteString("\n")
	}

	// Name tag
	sb.WriteString(fmt.Sprintf("  %s %s", RarityStars[d.companion.Rarity], d.companion.Name))

	return sb.String()
}

// RenderWithBubble returns the sprite alongside the speech bubble if active.
func (d *Display) RenderWithBubble() string {
	sprite := d.Render()
	quip := d.reactor.CurrentQuip()
	prefs := d.Preferences()

	if quip == nil {
		return sprite
	}

	spriteLines := strings.Split(sprite, "\n")
	bubble := renderBubble(quip.Text)
	bubbleLines := strings.Split(bubble, "\n")

	// Align bubble to the right of the sprite
	maxLines := len(spriteLines)
	if len(bubbleLines) > maxLines {
		maxLines = len(bubbleLines)
	}

	var sb strings.Builder
	for i := 0; i < maxLines; i++ {
		spriteLine := ""
		if i < len(spriteLines) {
			spriteLine = spriteLines[i]
		}
		bubbleLine := ""
		if i < len(bubbleLines) {
			bubbleLine = bubbleLines[i]
		}
		padded := padRight(spriteLine, 14)
		if prefs.Position == OverlayLeftBottom {
			sb.WriteString(bubbleLine)
			sb.WriteString(padded)
		} else {
			sb.WriteString(padded)
			sb.WriteString(bubbleLine)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func frameStepMs(intensity AnimationIntensity) int64 {
	switch intensity {
	case AnimationLow:
		return 1200
	case AnimationHigh:
		return 350
	default:
		return 700
	}
}

func heartStepMs(intensity AnimationIntensity) int64 {
	switch intensity {
	case AnimationLow:
		return 700
	case AnimationHigh:
		return 250
	default:
		return 500
	}
}

// renderBubble creates a speech bubble around text.
func renderBubble(text string) string {
	lines := wrapText(text, 28)
	maxWidth := 0
	for _, l := range lines {
		if len(l) > maxWidth {
			maxWidth = len(l)
		}
	}
	if maxWidth < 4 {
		maxWidth = 4
	}

	var sb strings.Builder
	// Top border
	sb.WriteString("╭")
	sb.WriteString(strings.Repeat("─", maxWidth+2))
	sb.WriteString("╮\n")
	// Content
	for _, l := range lines {
		sb.WriteString("│ ")
		sb.WriteString(padRight(l, maxWidth))
		sb.WriteString(" │\n")
	}
	// Bottom border
	sb.WriteString("╰")
	sb.WriteString(strings.Repeat("─", maxWidth+2))
	sb.WriteString("╯\n")
	// Tail
	sb.WriteString("  ╲\n")
	return sb.String()
}

func wrapText(text string, width int) []string {
	words := strings.Fields(text)
	var lines []string
	cur := ""
	for _, w := range words {
		if len(cur)+len(w)+1 > width && cur != "" {
			lines = append(lines, cur)
			cur = w
		} else {
			if cur != "" {
				cur += " "
			}
			cur += w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// MarkActivity records that the user just did something (resets idle timer).
func (d *Display) MarkActivity() {
	d.mu.Lock()
	d.lastActivity = time.Now()
	d.mu.Unlock()
}

// IdleChatCh returns the channel that emits proactive chat messages.
// The REPL should select on this channel to display idle chatter.
func (d *Display) IdleChatCh() <-chan string {
	return d.idleChatCh
}

// StopIdleChat stops the background idle chatter goroutine.
func (d *Display) StopIdleChat() {
	select {
	case d.stopIdle <- struct{}{}:
	default:
	}
}

// idleChatLoop runs in the background, periodically sending proactive chat.
func (d *Display) idleChatLoop() {
	rng := d.reactor.rng
	aiCount := 0

	for {
		// Random interval between 45s and 180s
		interval := time.Duration(45+rng.Intn(136)) * time.Second
		select {
		case <-time.After(interval):
		case <-d.stopIdle:
			return
		}

		d.mu.Lock()
		elapsed := time.Since(d.lastActivity)
		mood := d.Mood.Current()
		prefs := d.prefs
		chatBackend := d.ChatBackend
		d.mu.Unlock()

		if prefs.Mode == CompanionPractical {
			continue
		}

		// Only chat if user has been idle for at least 30s
		if elapsed < 30*time.Second {
			continue
		}

		var msg string

		// Every 3rd idle message, try AI-powered proactive message
		aiCount++
		if chatBackend != nil && aiCount%3 == 0 {
			event := fmt.Sprintf("用户已经空闲 %d 分钟了，当前时间 %s，我的心情是%s",
				int(elapsed.Minutes()), time.Now().Format("15:04"),
				string(mood))
			if aiMsg := chatBackend.ProactiveSuggestion(
				context.Background(), event); aiMsg != "" {
				msg = aiMsg
			}
		}

		// Fallback to static messages
		if msg == "" {
			msg = d.pickIdleChat(mood, elapsed)
		}
		if msg == "" {
			continue
		}

		// Non-blocking send
		select {
		case d.idleChatCh <- msg:
		default:
		}
	}
}

// pickIdleChat selects a proactive message based on mood and idle duration.
func (d *Display) pickIdleChat(mood Mood, idle time.Duration) string {
	rng := d.reactor.rng
	species := d.companion.Species
	hour := time.Now().Hour()

	// Mood influences chattiness: happy talks more, sleepy talks less
	switch mood {
	case MoodSleepy:
		if rng.Intn(100) < 70 {
			return "" // 70% chance to stay quiet when sleepy
		}
	case MoodHappy, MoodExcited:
		// Always chat when happy/excited
	default:
		if rng.Intn(100) < 30 {
			return "" // 30% chance to skip
		}
	}

	var pool []string

	// Time-aware messages
	if hour >= 0 && hour < 6 {
		pool = append(pool, lateNightChats...)
	} else if hour >= 6 && hour < 9 {
		pool = append(pool, morningChats...)
	} else if hour >= 12 && hour < 14 {
		pool = append(pool, lunchChats...)
	} else if hour >= 22 {
		pool = append(pool, lateNightChats...)
	}

	// Idle duration messages
	if idle > 5*time.Minute {
		pool = append(pool, longIdleChats...)
	} else {
		pool = append(pool, shortIdleChats...)
	}

	// Mood-specific
	switch mood {
	case MoodHappy, MoodExcited:
		pool = append(pool, happyChats...)
	case MoodAnnoyed:
		pool = append(pool, frustratedChats...)
	case MoodSleepy:
		pool = append(pool, tiredChats...)
	case MoodChill:
		pool = append(pool, curiousChats...)
	}

	// Species flavor
	pool = append(pool, speciesIdleChats(species)...)

	// General random thoughts
	pool = append(pool, randomThoughts...)

	if len(pool) == 0 {
		return ""
	}
	return pool[rng.Intn(len(pool))]
}

// Proactive chat message pools (Chinese)

var lateNightChats = []string{
	"这么晚了还在写代码啊……注意身体。",
	"*打了个哈欠* 夜深了呢。",
	"要不要休息一下？我可以帮你看着。",
	"凌晨的代码总是特别有灵感……也特别多 bug。",
	"月亮出来了，你看到了吗？",
}

var morningChats = []string{
	"早安！今天也要加油哦~",
	"*伸懒腰* 新的一天开始了！",
	"早起的虫子被鸟吃……等等，这个比喻不太对。",
	"咖啡喝了吗？",
	"今天天气怎么样？",
}

var lunchChats = []string{
	"该吃午饭了吧？别忘了。",
	"中午了，休息一下眼睛吧。",
	"*肚子咕噜叫*",
	"午饭想吃什么？",
}

var longIdleChats = []string{
	"还在吗？我有点无聊了……",
	"*四处张望*",
	"你去哪了？我一个人好寂寞。",
	"等你回来，我先练练翻跟头。",
	"是去泡茶了吗？也帮我倒一杯。",
	"*开始数天花板上的像素*",
	"我要开始唱歌了哦……不想听的话快回来。",
}

var shortIdleChats = []string{
	"在想什么呢？",
	"有什么我能帮忙的吗？",
	"*好奇地看着你*",
	"今天的进展如何？",
	"嘿~",
	"我刚才想到一个好点子……算了忘了。",
}

var happyChats = []string{
	"今天心情真好！",
	"*哼着小曲*",
	"写代码真开心~",
	"♪♪♪",
	"感觉今天特别顺利！",
}

var frustratedChats = []string{
	"别灰心，bug 总会解决的。",
	"我相信你可以的！",
	"要不换个思路试试？",
	"休息一下也许会有灵感。",
}

var tiredChats = []string{
	"*困了……*",
	"zZz……啊不，我没睡！",
	"*眼皮在打架*",
	"要不我们都休息一下？",
}

var curiousChats = []string{
	"你在做什么有趣的项目？",
	"这段代码好复杂，好厉害。",
	"*研究着屏幕上的代码*",
	"今天学到什么新东西了吗？",
}

var randomThoughts = []string{
	"你知道吗，1KB 是 1024 字节。",
	"如果 bug 是虫子，这项目已经是个动物园了。",
	"*自言自语* struct 还是 interface 呢……",
	"突然想到，递归就是……递归。",
	"tab 还是 space？永恒的命题。",
	"我觉得这段代码写得挺好的。",
	"*偷看你的代码*……看不懂，但尊重。",
	"今天的 commit message 想好了吗？",
	"要不要做个代码审查？我虽然帮不上忙但可以加油。",
	"你是更喜欢 vim 还是 emacs？……开玩笑的别打。",
	"*数了数* 这个文件有好多行呢。",
	"据说程序员的三大美德是懒惰、急躁和傲慢。",
}

func speciesIdleChats(species Species) []string {
	switch species {
	case Cat:
		return []string{
			"*舔爪子*",
			"*追着自己的尾巴转圈*",
			"我刚才看到一只虫子……是 bug 吗？",
			"*趴在你的键盘旁边*",
		}
	case Duck, Goose:
		return []string{
			"嘎？嘎嘎。",
			"*在代码旁边游来游去*",
			"你要不要跟我说说你的代码？橡皮鸭调试法~",
		}
	case Ghost:
		return []string{
			"*从屏幕里探出头*",
			"*安静地漂浮着*",
			"嘘……你听到了吗？哦没事，是我自己。",
		}
	case Dragon:
		return []string{
			"*吐了个小火圈*",
			"我的宝藏里要加上今天写的代码。",
			"*盘在显示器后面打盹*",
		}
	case Robot:
		return []string{
			"系统状态：正常。心情状态：想聊天。",
			"*处理器空转中*",
			"嘟嘟，检测到人类空闲。建议：互动。",
		}
	case Owl:
		return []string{
			"*歪了歪脑袋*",
			"我读了点文档，很有意思。",
			"咕咕，有个设计模式你可能会感兴趣……",
		}
	case Capybara:
		return []string{
			"*悠闲地泡着温泉*",
			"急什么，放松~",
			"*岁月静好地望着远方*",
		}
	case Mushroom:
		return []string{
			"*安静地生长着*",
			"*释放了一点孢子*",
			"我又长高了一点点。",
		}
	case Axolotl:
		return []string{
			"*晃着粉红色的腮须*",
			"水好舒服~",
			"*对你微笑*",
		}
	default:
		return nil
	}
}
