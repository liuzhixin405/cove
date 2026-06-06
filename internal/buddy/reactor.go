package buddy

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Event types that trigger buddy reactions.
type Event string

const (
	EventToolSuccess Event = "tool_success"
	EventToolError   Event = "tool_error"
	EventIdle        Event = "idle"
	EventTurn        Event = "turn_end" // fires after every text response
	EventStart       Event = "session_start"
	EventEnd         Event = "session_end"
	EventPet         Event = "pet"
	EventCompact     Event = "compact"
)

// Quip is a text reaction displayed in the speech bubble.
type Quip struct {
	Text    string
	Expires time.Time
}

// Reactor generates personality-based reactions for events.
type Reactor struct {
	mu        sync.Mutex
	Companion *Companion
	lastQuip  *Quip
	rng       *rand.Rand
	behavior  BehaviorPack
	mode      CompanionMode
}

// NewReactor creates a reactor for the given companion.
func NewReactor(c *Companion) *Reactor {
	return &Reactor{
		Companion: c,
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
		behavior:  BehaviorCoding,
		mode:      CompanionPractical,
	}
}

// SetBehaviorPack changes the behavior pack used for generated quips.
func (r *Reactor) SetBehaviorPack(pack BehaviorPack) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.behavior = NormalizePreferences(Preferences{Behavior: pack}).Behavior
}

// SetCompanionMode changes chatter strategy (practical vs playful).
func (r *Reactor) SetCompanionMode(mode CompanionMode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mode = NormalizePreferences(Preferences{Mode: mode}).Mode
}

// React generates a quip for the given event. May return nil (no reaction).
func (r *Reactor) React(event Event, detail string) *Quip {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Don't spam — skip if last quip is still showing
	if r.lastQuip != nil && time.Now().Before(r.lastQuip.Expires) {
		return nil
	}

	text := r.generateQuip(event, detail)
	if text == "" {
		return nil
	}

	q := &Quip{
		Text:    text,
		Expires: time.Now().Add(8 * time.Second),
	}
	r.lastQuip = q
	return q
}

// CurrentQuip returns the active quip if still visible.
func (r *Reactor) CurrentQuip() *Quip {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastQuip != nil && time.Now().Before(r.lastQuip.Expires) {
		return r.lastQuip
	}
	return nil
}

func (r *Reactor) generateQuip(event Event, detail string) string {
	c := r.Companion
	pack := r.behavior
	mode := r.mode
	snark := c.Stats[StatSnark]
	chaos := c.Stats[StatChaos]
	patience := c.Stats[StatPatience]

	if mode == CompanionPractical {
		return practicalQuip(event, detail, pack)
	}

	switch event {
	case EventToolSuccess:
		if r.rng.Intn(100) > 30 { // Only react 30% of the time
			return ""
		}
		opts := toolSuccessQuips(c.Species, snark)
		opts = append(opts, behaviorToolSuccessQuips(pack)...)
		return r.pickOne(opts)

	case EventToolError:
		if r.rng.Intn(100) > 60 { // React 60% to errors
			return ""
		}
		opts := toolErrorQuips(c.Species, snark, patience)
		opts = append(opts, behaviorToolErrorQuips(pack)...)
		return r.pickOne(opts)

	case EventIdle:
		if r.rng.Intn(100) > 15 { // Rarely speak when idle
			return ""
		}
		opts := idleQuips(c.Species, chaos)
		opts = append(opts, behaviorIdleQuips(pack)...)
		return r.pickOne(opts)

	case EventTurn:
		if r.rng.Intn(100) > 40 { // React 40% of the time after text responses
			return ""
		}
		opts := turnQuips(c.Species, snark, patience)
		opts = append(opts, behaviorTurnQuips(pack)...)
		return r.pickOne(opts)

	case EventStart:
		return r.pickOne(startQuips(c.Name, c.Species))

	case EventEnd:
		return r.pickOne(endQuips(c.Name, c.Species))

	case EventPet:
		return r.pickOne(petQuips(c.Species, snark))

	case EventCompact:
		if detail != "" {
			return fmt.Sprintf("*看着 %d 条消息消失了*", len(detail))
		}
		return r.pickOne([]string{"记忆……在消散……", "哦，大扫除时间！"})

	default:
		_ = chaos
		return ""
	}
}

func practicalQuip(event Event, detail string, pack BehaviorPack) string {
	tool := strings.TrimSpace(detail)
	if tool != "" {
		tool = "（" + tool + "）"
	}
	switch event {
	case EventStart:
		return "实用模式已启用：输入 /buddy tip 查看下一步。"
	case EventToolError:
		switch pack {
		case BehaviorReview:
			return "工具失败" + tool + "：先看输入与边界，再做最小回归。"
		case BehaviorDebug:
			return "工具失败" + tool + "：先最小复现，再加日志定位。"
		default:
			return "工具失败" + tool + "：建议拆小步骤重试。"
		}
	case EventCompact:
		return "上下文已压缩：如果中断，可输入“继续”。"
	default:
		return ""
	}
}

func (r *Reactor) pickOne(options []string) string {
	if len(options) == 0 {
		return ""
	}
	return options[r.rng.Intn(len(options))]
}

func toolSuccessQuips(species Species, snark int) []string {
	base := []string{
		"搞定了！",
		"成功~",
		"*满意地点头*",
		"漂亮。",
		"稳。",
	}
	if snark > 60 {
		base = append(base,
			"这我也能做到。",
			"总算搞定了。",
			"*慢慢鼓掌*",
		)
	}
	switch species {
	case Cat:
		base = append(base, "*呼噜呼噜*", "*把桌上东西推下去*")
	case Duck, Goose:
		base = append(base, "嘎！", "*赞许地拍翅膀*")
	case Ghost:
		base = append(base, "*兴奋地穿墙而过*", "漂亮~鬼才！")
	case Dragon:
		base = append(base, "*喷出小火苗表示赞许*", "宝藏又多了。")
	case Robot:
		base = append(base, "执行结果：最优", "嘟嘟，确认完毕。")
	}
	return base
}

func toolErrorQuips(species Species, snark, patience int) []string {
	base := []string{
		"呃……",
		"好像不太对。",
		"*担忧的表情*",
		"嗯？",
	}
	if snark > 70 {
		base = append(base,
			"试过重启吗？",
			"大胆的策略，看看效果如何。",
			"*记下什么不该做*",
		)
	}
	if patience > 60 {
		base = append(base,
			"没关系，再试一次！",
			"你可以的。",
			"错误也是学习嘛~",
		)
	}
	switch species {
	case Cat:
		base = append(base, "*审视的目光*", "*把错误推下桌子*")
	case Blob:
		base = append(base, "*紧张地抖动*")
	case Owl:
		base = append(base, "要不看看文档？", "*扶了扶眼镜*")
	case Snail:
		base = append(base, "慢……慢……来……")
	}
	return base
}

func idleQuips(species Species, chaos int) []string {
	base := []string{
		"*哈欠*",
		"……",
		"zZz",
		"*伸懒腰*",
	}
	if chaos > 60 {
		base = append(base,
			"好无聊，删掉node_modules吧。",
			"要不……把整个项目重构一下？",
			"*混乱地颤抖*",
		)
	}
	switch species {
	case Cat:
		base = append(base, "*趴在键盘上睡着了*", "*追逐光标*")
	case Duck:
		base = append(base, "*梳理羽毛*", "嘎？")
	case Ghost:
		base = append(base, "*忽明忽暗*", "*在终端里游荡*")
	case Mushroom:
		base = append(base, "*释放孢子*", "*微微长高了*")
	case Capybara:
		base = append(base, "*摆烂中*", "*岁月静好*")
	case Axolotl:
		base = append(base, "*晃动腮须*", "*平静地微笑*")
	}
	return base
}

func startQuips(name string, species Species) []string {
	base := []string{
		fmt.Sprintf("%s来啦！", name),
		"准备开工！",
		"*竖起耳朵*",
		"走起！",
	}
	switch species {
	case Cat:
		base = append(base, "*伸懒腰打哈欠*", "哦，你回来了。")
	case Duck:
		base = append(base, "嘎嘎嘎！新的一天！")
	case Robot:
		base = append(base, "系统已上线。", "伙伴模块激活。")
	case Dragon:
		base = append(base, "*展开翅膀*")
	case Penguin:
		base = append(base, "*兴奋地摇摆*")
	}
	return base
}

func endQuips(name string, species Species) []string {
	_ = name
	base := []string{
		"下次见！",
		"*挥手*",
		"拜拜~",
		"今天辛苦了！",
	}
	switch species {
	case Cat:
		base = append(base, "*蜷起来睡觉了*")
	case Ghost:
		base = append(base, "*渐渐消失*")
	case Robot:
		base = append(base, "进入休眠模式。")
	case Snail:
		base = append(base, "*慢慢缩回壳里*")
	}
	return base
}

func petQuips(species Species, snark int) []string {
	base := []string{
		"♡",
		"*开心的声音*",
		"*蹭过来*",
		":3",
	}
	if snark > 70 {
		base = append(base, "……行吧，摸吧。", "别搞得太奇怪啊。")
	}
	switch species {
	case Cat:
		base = append(base, "*呼噜噜噜*", "*用头蹭你*")
	case Duck:
		base = append(base, "*嘎嘎嘎！*", "*摇尾巴*")
	case Dragon:
		base = append(base, "*发出小火焰呼噜声*", "*危险地蹭过来*")
	case Ghost:
		base = append(base, "*你的手穿过去了*", "*内心暖暖的*")
	case Blob:
		base = append(base, "*被捏了一下*", "*开心地晃动*")
	case Robot:
		base = append(base, "收到好感。正在回馈。", "❤ +1")
	}
	return base
}

func turnQuips(species Species, snark, patience int) []string {
	base := []string{
		"*在听*",
		"嗯嗯。",
		"*点头*",
		"有意思……",
		"*认真地看着*",
	}
	if patience > 60 {
		base = append(base,
			"慢慢来~",
			"我在这儿呢。",
			"*耐心地眨眼*",
		)
	}
	if snark > 60 {
		base = append(base,
			"你知道我其实帮不上忙吧？",
			"*假装听懂了*",
			"嗯嗯，完全跟上了。",
		)
	}
	switch species {
	case Cat:
		base = append(base, "*抖了抖耳朵*", "*甩尾巴*", "*揉桌面*")
	case Duck:
		base = append(base, "*嘎*", "*歪头*")
	case Turtle:
		base = append(base, "*缓缓眨眼*", "*微微缩了一下*")
	case Ghost:
		base = append(base, "*安静地漂浮*", "*闪烁*")
	case Robot:
		base = append(base, "处理中……", "输入已确认。")
	case Capybara:
		base = append(base, "*发呆中*", "*岁月静好*")
	case Owl:
		base = append(base, "*智慧地点头*", "嗯，我明白了。")
	case Dragon:
		base = append(base, "*吐了个烟圈*", "*卷起尾巴*")
	case Mushroom:
		base = append(base, "*轻轻摇摆*")
	case Axolotl:
		base = append(base, "*开心地晃腮须*")
	}
	return base
}

func behaviorTurnQuips(pack BehaviorPack) []string {
	switch pack {
	case BehaviorReview:
		return []string{
			"先看边界条件。",
			"我先扫一遍风险点。",
			"命名和意图一致吗？",
		}
	case BehaviorDebug:
		return []string{
			"先最小复现，再下结论。",
			"日志里一定有线索。",
			"从错误出现前一跳开始看。",
		}
	default:
		return []string{
			"这段代码可以更顺滑。",
			"先写通，再打磨。",
			"我在盯着复杂度。",
		}
	}
}

func behaviorToolSuccessQuips(pack BehaviorPack) []string {
	switch pack {
	case BehaviorReview:
		return []string{"通过了，结构也挺干净。", "这次改动风险可控。"}
	case BehaviorDebug:
		return []string{"症状消失，继续观察。", "根因链路打通了。"}
	default:
		return []string{"完成一块积木。", "下一步可以并行推进。"}
	}
}

func behaviorToolErrorQuips(pack BehaviorPack) []string {
	switch pack {
	case BehaviorReview:
		return []string{"像是评审漏了一个前提。", "先补测试再改实现。"}
	case BehaviorDebug:
		return []string{"别慌，先抓第一条异常。", "我怀疑是状态没对齐。"}
	default:
		return []string{"迭代的一部分，继续。", "这块需要拆小一点。"}
	}
}

func behaviorIdleQuips(pack BehaviorPack) []string {
	switch pack {
	case BehaviorReview:
		return []string{"我在想有没有隐形耦合。"}
	case BehaviorDebug:
		return []string{"我去整理下可疑路径。"}
	default:
		return []string{"我在画下一步的草图。"}
	}
}

// PetHearts returns the floating hearts animation frames for /buddy pet.
var PetHearts = []string{
	"   ♥    ♥   ",
	"  ♥  ♥   ♥  ",
	" ♥   ♥  ♥   ",
	"♥  ♥      ♥ ",
	"·    ·   ·  ",
}

// FormatBuddyCard returns a formatted display of the buddy's stats.
func FormatBuddyCard(c *Companion) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("  %s  %s %s\n", RarityStars[c.Rarity], c.Name, RenderFace(c.Bones)))
	sb.WriteString(fmt.Sprintf("  种族: %s", c.Species))
	if c.Shiny {
		sb.WriteString(" ✨闪光")
	}
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("  性格: %s\n", c.Personality))
	sb.WriteString("\n  属性:\n")
	statCN := map[StatName]string{
		StatDebugging: "调试力",
		StatPatience:  "耐心值",
		StatChaos:     "混沌值",
		StatWisdom:    "智慧值",
		StatSnark:     "毒舌值",
	}
	for _, stat := range AllStats {
		bar := renderStatBar(c.Stats[stat])
		label := statCN[stat]
		if label == "" {
			label = string(stat)
		}
		sb.WriteString(fmt.Sprintf("    %-8s %s %d\n", label, bar, c.Stats[stat]))
	}
	return sb.String()
}

func renderStatBar(value int) string {
	filled := value / 10
	empty := 10 - filled
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", empty) + "]"
}
