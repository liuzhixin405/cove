package buddy

import (
	"fmt"
	"math/rand"
	"time"
)

// Mood represents the buddy's emotional state.
type Mood string

const (
	MoodHappy   Mood = "happy"
	MoodExcited Mood = "excited"
	MoodChill   Mood = "chill"
	MoodSleepy  Mood = "sleepy"
	MoodAnnoyed Mood = "annoyed"
	MoodProud   Mood = "proud"
)

var moodEmoji = map[Mood]string{
	MoodHappy:   "😊",
	MoodExcited: "✨",
	MoodChill:   "😌",
	MoodSleepy:  "💤",
	MoodAnnoyed: "😤",
	MoodProud:   "🌟",
}

var moodLabel = map[Mood]string{
	MoodHappy:   "开心",
	MoodExcited: "兴奋",
	MoodChill:   "悠闲",
	MoodSleepy:  "犯困",
	MoodAnnoyed: "烦躁",
	MoodProud:   "骄傲",
}

// MoodEmoji returns the emoji for the mood.
func MoodEmoji(m Mood) string { return moodEmoji[m] }

// MoodLabel returns the Chinese label for the mood.
func MoodLabel(m Mood) string { return moodLabel[m] }

// MoodTracker tracks the buddy's mood based on recent events.
type MoodTracker struct {
	current     Mood
	successRun  int // consecutive successes
	errorRun    int // consecutive errors
	idleTicks   int
	lastEventAt time.Time
}

// NewMoodTracker starts in chill mood.
func NewMoodTracker() *MoodTracker {
	return &MoodTracker{
		current:     MoodChill,
		lastEventAt: time.Now(),
	}
}

// Current returns the current mood.
func (m *MoodTracker) Current() Mood { return m.current }

// OnEvent updates mood based on events.
func (m *MoodTracker) OnEvent(event Event) {
	m.lastEventAt = time.Now()

	switch event {
	case EventToolSuccess:
		m.errorRun = 0
		m.idleTicks = 0
		m.successRun++
		if m.successRun >= 3 {
			m.current = MoodProud
		} else {
			m.current = MoodHappy
		}
	case EventToolError:
		m.successRun = 0
		m.idleTicks = 0
		m.errorRun++
		if m.errorRun >= 3 {
			m.current = MoodAnnoyed
		}
	case EventStart:
		m.current = MoodExcited
		m.successRun = 0
		m.errorRun = 0
	case EventPet:
		m.current = MoodHappy
		m.errorRun = 0
	case EventIdle:
		m.idleTicks++
		if m.idleTicks > 5 {
			m.current = MoodSleepy
		}
	case EventTurn:
		m.idleTicks = 0
		if m.current == MoodSleepy {
			m.current = MoodChill
		}
	}
}

// DailyFortune generates a deterministic "daily fortune" based on date + user.
type DailyFortune struct {
	seed string
}

// NewDailyFortune creates a daily fortune tied to a user.
func NewDailyFortune(userID string) *DailyFortune {
	return &DailyFortune{seed: userID}
}

var fortunes = []string{
	"今天代码一次编译通过的概率：87%",
	"今日幸运语言：Go",
	"宜：写测试。忌：rm -rf",
	"今日 bug 运：低。放心大胆写！",
	"今日适合重构那段你一直想动的代码",
	"今日幸运数字：42",
	"宜：提交代码。忌：合并冲突",
	"今天的 commit message 会特别优雅",
	"今日适合学一个新 API",
	"Bug 会主动找上门来——准备好 debugger",
	"今天的代码会被未来的你夸奖",
	"宜：pair programming。忌：solo debug",
	"今日灵感指数：MAX",
	"今天适合给变量起好名字",
	"今日宜摸鱼 5 分钟再开工",
	"Stack Overflow 今天对你格外友好",
	"今日幸运符号：;",
	"今天写的函数会特别简洁",
	"宜：写文档。忌：删注释",
	"今日代码味道：清新薄荷",
}

// Today returns today's fortune.
func (f *DailyFortune) Today() string {
	now := time.Now()
	daySeed := hashString(fmt.Sprintf("%s-%d-%d-%d", f.seed, now.Year(), now.Month(), now.Day()))
	return fortunes[int(daySeed)%len(fortunes)]
}

// XPTracker manages buddy experience and levels.
type XPTracker struct {
	XP    int
	Level int
}

var levelThresholds = []int{0, 10, 30, 60, 100, 150, 220, 300, 400, 500, 650, 800, 999}

var levelTitles = []string{
	"蛋壳碎片", // 0
	"小萌新",  // 1
	"初出茅庐", // 2
	"代码学徒", // 3
	"靠谱搭档", // 4
	"得力助手", // 5
	"编程伙伴", // 6
	"资深搭档", // 7
	"代码大师", // 8
	"传说之友", // 9
	"终极陪伴", // 10
	"超越存在", // 11
	"∞",    // 12
}

// NewXPTracker creates a tracker from stored XP.
func NewXPTracker(xp int) *XPTracker {
	t := &XPTracker{XP: xp}
	t.recalcLevel()
	return t
}

func (t *XPTracker) recalcLevel() {
	t.Level = 0
	for i, thresh := range levelThresholds {
		if t.XP >= thresh {
			t.Level = i
		}
	}
}

// Add adds XP and returns true if leveled up.
func (t *XPTracker) Add(amount int) bool {
	oldLevel := t.Level
	t.XP += amount
	t.recalcLevel()
	return t.Level > oldLevel
}

// Title returns the current level title.
func (t *XPTracker) Title() string {
	if t.Level < len(levelTitles) {
		return levelTitles[t.Level]
	}
	return levelTitles[len(levelTitles)-1]
}

// NextLevelXP returns XP needed for next level, or -1 if max.
func (t *XPTracker) NextLevelXP() int {
	next := t.Level + 1
	if next >= len(levelThresholds) {
		return -1
	}
	return levelThresholds[next] - t.XP
}

// ProgressBar returns a visual progress bar for current level.
func (t *XPTracker) ProgressBar() string {
	next := t.Level + 1
	if next >= len(levelThresholds) {
		return "[██████████] MAX"
	}
	prev := levelThresholds[t.Level]
	needed := levelThresholds[next] - prev
	current := t.XP - prev
	filled := current * 10 / needed
	if filled > 10 {
		filled = 10
	}
	empty := 10 - filled
	return fmt.Sprintf("[%s%s] %d/%d",
		repeat("█", filled), repeat("░", empty),
		current, needed)
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// PetCombo tracks rapid-fire petting for easter eggs.
type PetCombo struct {
	count    int
	lastPet  time.Time
	cooldown time.Duration
}

// NewPetCombo creates a combo tracker.
func NewPetCombo() *PetCombo {
	return &PetCombo{cooldown: 3 * time.Second}
}

// Hit registers a pet action. Returns combo count and any special reaction.
func (p *PetCombo) Hit() (int, string) {
	now := time.Now()
	if now.Sub(p.lastPet) > p.cooldown {
		p.count = 0
	}
	p.count++
	p.lastPet = now

	switch {
	case p.count == 5:
		return p.count, "连击！*转圈圈*"
	case p.count == 10:
		return p.count, "超级连击！！*飞起来了*"
	case p.count == 15:
		return p.count, "无敌连击！！！*变成彩虹色*"
	case p.count >= 20:
		return p.count, petEasterEggs[rand.Intn(len(petEasterEggs))]
	case p.count == 3:
		return p.count, "*尾巴摇得更快了*"
	default:
		return p.count, ""
	}
}

var petEasterEggs = []string{
	"*解锁了隐藏舞蹈*  ♪┏(・o・)┛♪",
	"*分裂成两个自己* 等等什么？？",
	"*突然戴上了墨镜* 😎",
	"*唱起了歌* ♪ 叮叮当~ ♪",
	"*化身为巨大版本* 「嗯？」",
	"*时空扭曲* 你确定要继续摸吗……",
	"成就解锁：疯狂撸猫人",
	"*召唤出了小星星* ⭐⭐⭐",
}

// TimeGreeting returns a time-appropriate greeting.
func TimeGreeting() string {
	hour := time.Now().Hour()
	switch {
	case hour >= 5 && hour < 9:
		return "早安！新的一天开始啦~"
	case hour >= 9 && hour < 12:
		return "上午好！精神满满~"
	case hour >= 12 && hour < 14:
		return "午饭时间到了没？"
	case hour >= 14 && hour < 18:
		return "下午好~继续加油！"
	case hour >= 18 && hour < 21:
		return "晚上好！今天辛苦了~"
	case hour >= 21 && hour < 23:
		return "夜深了，注意休息哦。"
	default:
		return "这么晚还在写代码？注意身体！"
	}
}

// TimeEmoji returns an emoji for the current time of day.
func TimeEmoji() string {
	hour := time.Now().Hour()
	switch {
	case hour >= 6 && hour < 18:
		return "☀️"
	case hour >= 18 && hour < 21:
		return "🌆"
	default:
		return "🌙"
	}
}
