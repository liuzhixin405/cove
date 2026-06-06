import re

def fix_file(path, replacements):
    with open(path, 'r', encoding='utf-8') as f:
        content = f.read()
    
    for old, new in replacements.items():
        if isinstance(old, re.Pattern):
            content = old.sub(new, content)
        else:
            content = content.replace(old, new)
            
    with open(path, 'w', encoding='utf-8') as f:
        f.write(content)

repl = {
    'err = fmt.Errorf("当前运行器不支持附件消\\n息")': 'err = fmt.Errorf("当前运行器不支持附件消息")',
    'note := fmt.Sprintf("\\n网络波动 ，自动重试\\n中 (%d/%d)...\\n", attempt, maxAttempts)': 'note := fmt.Sprintf("\\n网络波动，自动重试中 (%d/%d)...\\n", attempt, maxAttempts)',
    'return fmt.Sprintf("预算已超限，继续重试\\n不会成功。可执行 /budget auto 一键提高到 $%.2f，或手动 /budget <金额>，然后再输\\n入“继续”。", suggested)': 'return fmt.Sprintf("预算已超限，继续重试不会成功。可执行 /budget auto 一键提高到 $%.2f，或手动 /budget <金额>，然后再输入“继续”。", suggested)',
    'return "预算已超限，继续重试不会成功。请先\\n执行 /budget auto 或 /budget <更大金额>，然后再输入“继续”。"': 'return "预算已超限，继续重试不会成功。请先执行 /budget auto 或 /budget <更大金额>，然后再输入“继续”。"'
}
fix_file('cmd/agentgo/chat_interaction.go', repl)

repl = {
    'cmdEntry{Name: "/attach", Desc: "挂载图 片或文件\\n到后续提问", Type: "builtin", ArgHints: map[string][]string{"": {"list", "clear\\n", "remove", "add"}}},': 'cmdEntry{Name: "/attach", Desc: "挂载图片或文件到后续提问", Type: "builtin", ArgHints: map[string][]string{"": {"list", "clear", "remove", "add"}}},'
}
fix_file('cmd/agentgo/completion.go', repl)

repl = {
    'fmt.Println("附件输入: 在 REPL 或 -p 文本中可写 @\\n路径，例如：解释这张图 @assets/screen.png")': 'fmt.Println("附件输入: 在 REPL 或 -p 文本中可写 @路径，例如：解释这张图 @assets/screen.png")',
    '"最快的办法：直接在当前 REPL 输 入\\n"+': '"最快的办法：直接在当前 REPL 输入\\n"+'
}
fix_file('cmd/agentgo/repl_help.go', repl)

repl = {
    'repl.PrintSafe("暂无历史。退出时会自动保存会\\n话。\\n")': 'repl.PrintSafe("暂无历史。退出时会自动保存会话。\\n")',
    'repl.PrintSafe("已自动恢复最近有效任务 #%d: %s\\n (%d 条消息)\\n", best.idx, title, len(best.rec.Messages))': 'repl.PrintSafe("已自动恢复最近有效任务 #%d: %s (%d 条消息)\\n", best.idx, title, len(best.rec.Messages))'
}
fix_file('cmd/agentgo/repl_history.go', repl)

repl = {
    'repl.PrintAbove(fmt.Sprintf("\\r\\n%s任务执行出\\n现内部异常，已恢复输入。可输入“继续”重试。%s\\r\\n", repl.Red, repl.Reset))': 'repl.PrintAbove(fmt.Sprintf("\\r\\n%s任务执行出现内部异常，已恢复输入。可输入“继续”重试。%s\\r\\n", repl.Red, repl.Reset))',
    'repl.PrintAbove("可输入“继续”重 试刚才中断的\\n任务。\\n")': 'repl.PrintAbove("可输入“继续”重试刚才中断的任务。\\n")'
}
fix_file('cmd/agentgo/repl_tasks.go', repl)

