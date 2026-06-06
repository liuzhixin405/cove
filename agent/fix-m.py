import re
content = open("cmd/agentgo/main.go", "r", encoding="utf-8").read()
lines = content.split("\n")
for i, line in enumerate(lines):
    if ("fmt.Println" in line or "fmt.Printf" in line) and line.rstrip().endswith("?)"):
        lines[i] = line.rstrip()[:-2] + '")'
content = "\n".join(lines)
open("cmd/agentgo/main.go", "w", encoding="utf-8").write(content)

