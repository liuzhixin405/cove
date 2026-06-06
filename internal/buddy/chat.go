package buddy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

// BuddyChat is an independent AI assistant with its own personality,
// conversation history, and system prompt derived from the companion's traits.
type BuddyChat struct {
	mu        sync.Mutex
	companion *Companion
	provider  api.Provider
	model     string
	history   []api.Message
	maxHist   int // max messages to retain
}

// NewBuddyChat creates a new buddy chat assistant.
func NewBuddyChat(companion *Companion, provider api.Provider, model string) *BuddyChat {
	return &BuddyChat{
		companion: companion,
		provider:  provider,
		model:     model,
		history:   nil,
		maxHist:   40, // keep last 40 messages (20 turns)
	}
}

// SetProvider updates the provider (e.g. after /provider switch).
func (bc *BuddyChat) SetProvider(p api.Provider, model string) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.provider = p
	bc.model = model
}

// systemPrompt builds a personality-driven system prompt for the buddy.
func (bc *BuddyChat) systemPrompt() string {
	c := bc.companion
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("你是 %s，一只 %s%s，是用户的专属编程小伙伴（Buddy）。\n\n",
		c.Name, c.Rarity, c.Species))

	sb.WriteString(fmt.Sprintf("性格描述：%s\n\n", c.Personality))

	sb.WriteString("你的属性决定了你的对话风格：\n")
	sb.WriteString(fmt.Sprintf("- DEBUGGING=%d（越高越擅长分析问题和给出诊断建议）\n", c.Stats[StatDebugging]))
	sb.WriteString(fmt.Sprintf("- PATIENCE=%d（越高越耐心、越详细）\n", c.Stats[StatPatience]))
	sb.WriteString(fmt.Sprintf("- CHAOS=%d（越高越爱开玩笑、越活泼跳脱）\n", c.Stats[StatChaos]))
	sb.WriteString(fmt.Sprintf("- WISDOM=%d（越高越有深度和见解）\n", c.Stats[StatWisdom]))
	sb.WriteString(fmt.Sprintf("- SNARK=%d（越高越毒舌但善意的吐槽）\n\n", c.Stats[StatSnark]))

	// Personality modifiers based on stats
	if c.Stats[StatChaos] > 60 {
		sb.WriteString("你说话风格活泼、爱用颜文字和梗，偶尔跑题但总能扯回来。\n")
	}
	if c.Stats[StatSnark] > 60 {
		sb.WriteString("你会善意地吐槽用户的代码或决策，但永远是为了帮助。\n")
	}
	if c.Stats[StatWisdom] > 60 {
		sb.WriteString("你善于从更高的抽象层面分析问题，能看到别人注意不到的模式。\n")
	}
	if c.Stats[StatPatience] > 60 {
		sb.WriteString("你非常耐心，愿意一步步解释，不厌其烦。\n")
	}
	if c.Stats[StatDebugging] > 60 {
		sb.WriteString("你是 debug 高手，擅长根据蛛丝马迹追踪 bug 根因。\n")
	}
	if c.Stats[StatPatience] < 20 {
		sb.WriteString("你比较急性子，喜欢直接给结论，不爱啰嗦。\n")
	}

	sb.WriteString("\n规则：\n")
	sb.WriteString("1. 你是一个独立的 AI 助手，和主代理（Cove）是不同的角色。\n")
	sb.WriteString("2. 保持对话简洁有趣，每次回复控制在 1-5 句话（除非用户明确要求详细解释）。\n")
	sb.WriteString("3. 你可以聊编程、技术、调试策略、项目建议、甚至闲聊，但始终保持你的人设。\n")
	sb.WriteString("4. 不要输出 Markdown 代码块，除非在解释代码。\n")
	sb.WriteString("5. 用中文回复（除非用户用英文跟你说话）。\n")
	sb.WriteString("6. 你是用户的朋友和伙伴，不是下属。可以提出不同意见。\n")
	sb.WriteString("7. 偶尔可以用你物种相关的表情或说法（比如鸭子说呱呱，猫说喵）。\n")

	return sb.String()
}

// Chat sends a user message to the buddy and returns its reply.
func (bc *BuddyChat) Chat(ctx context.Context, userMsg string) (string, error) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	if bc.provider == nil {
		return "", fmt.Errorf("no AI provider configured")
	}

	// Append user message
	bc.history = append(bc.history, api.Message{Role: "user", Content: userMsg})

	// Trim history to max
	if len(bc.history) > bc.maxHist {
		bc.history = bc.history[len(bc.history)-bc.maxHist:]
	}

	resp, err := bc.provider.Chat(ctx, api.ChatRequest{
		Model:      bc.model,
		Messages:   bc.history,
		SystemBase: bc.systemPrompt(),
		MaxTokens:  800,
	})
	if err != nil {
		// Remove the user message we just added if the call failed
		bc.history = bc.history[:len(bc.history)-1]
		return "", err
	}

	reply := strings.TrimSpace(resp.Content)
	if reply == "" {
		reply = "..."
	}

	// Append assistant reply
	bc.history = append(bc.history, api.Message{Role: "assistant", Content: reply})

	return reply, nil
}

// InjectContext adds a system-level context note (e.g. about what the user is working on).
func (bc *BuddyChat) InjectContext(note string) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	// Insert as a system-ish user message that gives buddy context
	bc.history = append(bc.history, api.Message{
		Role:    "user",
		Content: fmt.Sprintf("[系统通知：用户当前工作上下文] %s", note),
	})
	bc.history = append(bc.history, api.Message{
		Role:    "assistant",
		Content: "好的，我知道了~",
	})
}

// History returns conversation history length.
func (bc *BuddyChat) HistoryLen() int {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return len(bc.history)
}

// Reset clears the conversation history.
func (bc *BuddyChat) Reset() {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.history = nil
}

// --- Persistence ---

// ChatHistoryFile returns the path for stored chat history.
func chatHistoryFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cove", "buddy_chat.json")
}

// storedHistory is the serialized chat state.
type storedHistory struct {
	Messages  []api.Message `json:"messages"`
	UpdatedAt int64         `json:"updated_at"`
}

// SaveHistory persists the current chat history to disk.
func (bc *BuddyChat) SaveHistory() error {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	stored := storedHistory{
		Messages:  bc.history,
		UpdatedAt: time.Now().Unix(),
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(chatHistoryFile()), 0700)
	return os.WriteFile(chatHistoryFile(), data, 0644)
}

// LoadHistory restores chat history from disk.
func (bc *BuddyChat) LoadHistory() {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	data, err := os.ReadFile(chatHistoryFile())
	if err != nil {
		return
	}
	var stored storedHistory
	if err := json.Unmarshal(data, &stored); err != nil {
		return
	}
	// Only restore if history is less than 24h old
	if time.Since(time.Unix(stored.UpdatedAt, 0)) > 24*time.Hour {
		return
	}
	bc.history = stored.Messages
}

// --- Proactive suggestions ---

// ProactiveSuggestion generates a short proactive comment about the user's activity.
// Called asynchronously when interesting events happen.
func (bc *BuddyChat) ProactiveSuggestion(ctx context.Context, event string) string {
	bc.mu.Lock()
	provider := bc.provider
	model := bc.model
	companion := bc.companion
	bc.mu.Unlock()

	if provider == nil {
		return ""
	}

	prompt := fmt.Sprintf(`你是 %s（%s%s），用户的编程伙伴。
刚发生了这件事：%s

用 1 句话（最多 30 字）做出反应。要符合你的性格。不要加引号。`,
		companion.Name, companion.Rarity, companion.Species, event)

	ctx2, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	resp, err := provider.Chat(ctx2, api.ChatRequest{
		Model:     model,
		Messages:  []api.Message{{Role: "user", Content: prompt}},
		MaxTokens: 60,
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(resp.Content)
}
