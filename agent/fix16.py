import re

with open('cmd/agentgo/main.go', 'r', encoding='utf-8') as f:
    text = f.read()

text = text.replace('fmt.Printf("[宸茶ˉ鍏匽 鍏堝墠浠诲姟杩樺湪璺戯紝鍓嶉潰鎺掗槦: %d", queuedAhead)', 'fmt.Printf("[已补充] 先前任务还在跑，前面排队: %d\\n", queuedAhead)')
text = text.replace('fmt.Println("[宸茶ˉ鍏匽 宸插悎骞惰繘褰撳墠澶勭悊浠诲姟")', 'fmt.Println("[已补充] 已合并进当前处理任务")')

with open('cmd/agentgo/main.go', 'w', encoding='utf-8') as f:
    f.write(text)
