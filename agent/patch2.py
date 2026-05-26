
import re

with open("d:/code/cagentcli-main/cagentcli-main/agent/cmd/agentgo/main.go", "r", encoding="utf-8") as f:
    text = f.read()

text = re.sub(r"fmt\.Print\(\"[\r\n]+\[Interrupted\][\r\n]+\"\)", "fmt.Print(\"\\\\r\\\\n[Interrupted]\\\\r\\\\n\")", text)

with open("d:/code/cagentcli-main/cagentcli-main/agent/cmd/agentgo/main.go", "w", encoding="utf-8") as f:
    f.write(text)

