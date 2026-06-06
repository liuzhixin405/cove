import re

with open('cmd/agentgo/main.go', 'r', encoding='utf-8') as f:
    text = f.read()

text = text.replace('fmt.Println("[杈撳叆宸叉帴鏀禲")', 'fmt.Println("[输入已接收]")')
text = text.replace('fmt.Printf("[浠诲姟鎺掗槦涓璢 鍓嶆柟鎺掗槦鏁? %d", queuedAhead)', 'fmt.Printf("[任务排队中] 前方排队数: %d\\n", queuedAhead)')

with open('cmd/agentgo/main.go', 'w', encoding='utf-8') as f:
    f.write(text)
