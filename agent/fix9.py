import re

with open('cmd/agentgo/main.go', 'r', encoding='utf-8') as f:
    text = f.read()

text = text.replace('fmt.Println("X_X_X\n提示: 在 prompt'.replace('X_X_X', ''), 'fmt.Println("\\n提示: 在 prompt')

with open('cmd/agentgo/main.go', 'w', encoding='utf-8') as f:
    f.write(text)
