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
    'return fmt.Sprintf("Console code pag\\ne warning / 控制台代码页提醒: input CP=%d, output CP=%d. Current console is not\\n full UTF-8, so Chinese input/output may still look garbled. Run chcp 65001 bef\\nore starting agentgo, or use Windows Terminal / UTF-8. 当前控制台还不是完整 UTF\\n-8，所以中文输入输出仍可能乱码。请先执行 chcp 65001，再启动 agentgo，或直接使用\\n Windows Terminal / UTF-8。", inputCP, outputCP)': 'return fmt.Sprintf("Console code page warning / 控制台代码页提醒: input CP=%d, output CP=%d. Current console is not full UTF-8, so Chinese input/output may still look garbled. Run chcp 65001 before starting agentgo, or use Windows Terminal / UTF-8. 当前控制台还不是完整 UTF-8，所以中文输入输出仍可能乱码。请先执行 chcp 65001，再启动 agentgo，或直接使用 Windows Terminal / UTF-8。", inputCP, outputCP)'
}
fix_file('cmd/agentgo/windows_console_windows.go', repl)

repl = {
    'Text: fmt.Sprintf("[图片附件 %s 已自动降\\n级：当前模型 %s 可能不支持视觉输入。请切换视觉模型后重试。]", name, model),': 'Text: fmt.Sprintf("[图片附件 %s 已自动降级：当前模型 %s 可能不支持视觉输入。请切换视觉模型后重试。]", name, model),',
    'return fmt.Errorf("API error %d: 当前接口\\n不支持图片输入(image_url)。请移除附件或切换支持视觉的模型/端点。原始错误: %s", \\nstatus, truncate(msg, 300))': 'return fmt.Errorf("API error %d: 当前接口不支持图片输入(image_url)。请移除附件或切换支持视觉的模型/端点。原始错误: %s", status, truncate(msg, 300))'
}
fix_file('internal/api/openai_compat.go', repl)
