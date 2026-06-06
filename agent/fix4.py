import re

with open('cmd/agentgo/main.go', 'r', encoding='utf-8') as f:
    lines = f.readlines()

for i, line in enumerate(lines):
    if 'fmt.Println(' in line and 'agentgo' in line and 'Go' in line and 'AI' in line:
        pass # wait I can just use git checkout on the file? Wait, no.

