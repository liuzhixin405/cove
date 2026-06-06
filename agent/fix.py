import sys
with open('cmd/agentgo/main.go', 'r', encoding='utf-8') as f:
    lines = f.readlines()
for i, line in enumerate(lines):
    if '阎庤鐡旷亸顏堫敋' in line:
        lines[i] = ' ' * 9 + 'fmt.Println(\"[系统] 等待任务停止...\")\n'
    if '阎熸粎澧楅幐鍛婃櫠' in line:
        lines[i] = ' ' * 8 + 'fmt.Println(\"[提示] 当前有任务正在运行，请等待其结束后再重试。\")\n'
    if '濠碘槅鍋€閸嬫挻绻涢弶鎴剰' in line:
        if 'handleHistoryResumeMostRelevant' in lines[i+1] or 'handleHistoryResumeMostRelevant' in lines[i+2] or 'handleHistoryResumeMostRelevant' in lines[i+3]:
            lines[i] = ' ' * 10 + 'fmt.Println(\"[提示] 已为您推荐相关历史任务...\")\n'
        else:
            lines[i] = ' ' * 10 + 'fmt.Println(\"[恢复] 任务记录已合并。\")\n'
    if '阎庣懓鎲¤ぐ鍐╂叏' in line:
        lines[i] = ' ' * 10 + 'fmt.Println(\"[恢复] 任务已排队，即将开始处理。\")\n'
with open('cmd/agentgo/main.go', 'w', encoding='utf-8') as f:
    f.writelines(lines)
