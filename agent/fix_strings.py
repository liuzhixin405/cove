
lines = open("cmd/agentgo/main.go", "r", encoding="utf-8").read().split("\n")
lines[565-1] = "s = string(runes[:maxLen-1]) + \"...\""
lines[704-1] = "installed = \" [installed]\""
lines[747-1] = "repl.PrintSafe(\"\\n[Skill %s]\\n\\n%s\\n\\n\", name, prompt)"
lines[783-1] = "repl.PrintSafe(\"\\n\")"
with open("cmd/agentgo/main.go", "w", encoding="utf-8") as f:
    f.write("\n".join(lines))

