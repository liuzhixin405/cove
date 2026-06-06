
lines = open("internal/repl/readline.go", "r", encoding="utf-8").read().split("\n")
lines[70-1] = "placeholder: \"(按 / 显示命令)\","
lines[221-1] = "// 关键点：在按下回车后，先把用户输入的内容打印到终端，使之成为历史可见内容"
with open("internal/repl/readline.go", "w", encoding="utf-8") as f:
    f.write("\n".join(lines))

