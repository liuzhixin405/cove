import re
with open("d:/code/cagentcli-main/cagentcli-main/agent/cmd/agentgo/main.go", "r", encoding="utf-8") as f:
    text = f.read()

text = re.sub(r"handleCommand\(ctx, input, cmdReg, cfg, eng, mcpPool, skillMgr, memStore, pluginMgr, pm, projCtx, as\)", "withInterrupt(func(ctx context.Context) { handleCommand(ctx, input, cmdReg, cfg, eng, mcpPool, skillMgr, memStore, pluginMgr, pm, projCtx, as) })", text)

text = re.sub(r"fmt\.Print\(runChatInteraction\(ctx, eng, input\)\)", "withInterrupt(func(ctx context.Context) { fmt.Print(runChatInteraction(ctx, eng, input)) })", text)

with open("d:/code/cagentcli-main/cagentcli-main/agent/cmd/agentgo/main.go", "w", encoding="utf-8") as f:
    f.write(text)

