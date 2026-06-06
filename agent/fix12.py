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

replacements = {
    'fmt.Println("当前没有挂载附件。用 /attach <文\\n件...> 添加。")': 'fmt.Println("当前没有挂载附件。用 /attach <文件...> 添加。")',
    'return "", fmt.Errorf("路径是目录，不是 文件")': 'return "", fmt.Errorf("路径是目录，不是文件")',
    'return api.MessagePart{}, "", "", fmt.Errorf("\\n附件路径是目录，不是文件: %s", rawPath)': 'return api.MessagePart{}, "", "", fmt.Errorf("附件路径是目录，不是文件: %s", rawPath)',
    '"文件过大 (%.1fMB > %.0fMB 限制): %s\\n  提示:\\n 请先用图片工具缩小尺寸后再挂载",': '"文件过大 (%.1fMB > %.0fMB 限制): %s\\n  提示: 请先用图片工具缩小尺寸后再挂载",',
    'warning = fmt.Sprintf("⚠ 当前模型 %s 可 能不支\\n持图片视觉功能，已自动降级为文本提示。建议切换到视觉模型 (如 deepseek-chat / gp\\nt-4o / claude-sonnet-4)", model)': 'warning = fmt.Sprintf("⚠ 当前模型 %s 可能不支持图片视觉功能，已自动降级为文本提示。建议切换到视觉模型 (如 deepseek-chat / gpt-4o / claude-sonnet-4)", model)',
    'Text:     fmt.Sprintf("[已挂载图片 %s，但当前\\n模型 %s 可能不支持视觉输入，图片内容未发送。请切换视觉模型后重试。]", name, mod\\nel),': 'Text:     fmt.Sprintf("[已挂载图片 %s，但当前模型 %s 可能不支持视觉输入，图片内容未发送。请切换视觉模型后重试。]", name, model),',
    '"图片过大且无法压缩 (%.1fMB > 5MB): %s\\n  提\\n示: 请手动缩小图片后再挂载，或用截图工具截取更小的区域",': '"图片过大且无法压缩 (%.1fMB > 5MB): %s\\n  提示: 请手动缩小图片后再挂载，或用截图工具截取更小的区域",',
    '"图片压缩后仍然过大 (%.1fMB): %s\\n  提示: 请\\n减小图片尺寸后再挂载",': '"图片压缩后仍然过大 (%.1fMB): %s\\n  提示: 请减小图片尺寸后再挂载",',
    'Text:     fmt.Sprintf("附件 %s (%s) 为二进制文\\n件，以下为 base64 片段%s:\\n%s", name, mimeType, truncated, encoded),': 'Text:     fmt.Sprintf("附件 %s (%s) 为二进制文件，以下为 base64 片段%s:\\n%s", name, mimeType, truncated, encoded),',
}

fix_file('cmd/agentgo/attachments.go', replacements)
