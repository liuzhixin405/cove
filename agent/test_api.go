package main
import (
"encoding/json"
"fmt"
)

type toolCall struct {
ID string `json:"id"`
}
type oaiMsg struct {
Role      string     `json:"role"`
Content   string     `json:"content"`
ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

func main() {
m := oaiMsg{Role: "assistant", Content: ""}
b, _ := json.Marshal(m)
fmt.Println("Empty:", string(b))
}
