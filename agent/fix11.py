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
    'repl.PrintAbove(fmt.Sprintf("  %s(闂佸搫鐗滄禍锝囨椤撱垹绀傞柕澶樺灣缁€澶婎潡濞戞瑯鐒炬い鎾愁煼楠炲繘骞掗弮鍌氬椽)%s\\n", repl.Dim, repl.Reset))': 'repl.PrintAbove(fmt.Sprintf("  %s(按 y 确认，n 拒绝，a 全部接受)%s\\n", repl.Dim, repl.Reset))',
    'repl.PrintSafe("闂佺懓鐏堥崑鎾绘煠鐎圭姵顥夌紒鏂跨埣瀹曢攱娼幍顔炬喒闂佸憡鐟崹鎶藉极? %v\\n", err)': 'repl.PrintSafe("获取技能市场列表失败: %v\\n", err)',
    'repl.PrintSafe("阎庣懓鎲¤ぐ鍐偩閵娧勫晳? %s 闂?闂備焦褰冪粔鎾箚鎼淬劌瑙﹂幖杈剧稻閺呮悂鏌℃担鍝ュ⒊缂佽鲸绻堥獮瀣冀椤愶絿鍑介梺瑙勪航閸庡娆㈤銏犵?/skill %s\\n", name, name)': 'repl.PrintSafe("成功安装技能 %s！现在可以使用 /skill %s 调用它。\\n", name, name)',
    'repl.PrintSafe("阎庣懓鎲¤ぐ鍐垂閸楃儐鍤? %s\\n缂傚倸鍊归悧鐐垫?~/.agentgo/skills/%s/SKILL.md\\n", name, name)': 'repl.PrintSafe("成功创建本地技能目录 %s，请编辑 ~/.agentgo/skills/%s/SKILL.md\\n", name, name)',
    'repl.PrintSafe("阎庣懓鎲¤ぐ鍐垂閸楃儐鍤? %s 闂?缂傚倸鍊归悧鐐垫?~/.agentgo/skills/%s/SKILL.md\\n", name, name)': 'repl.PrintSafe("成功创建本地技能目录 %s，请编辑 ~/.agentgo/skills/%s/SKILL.md\\n", name, name)',
    'repl.PrintSafe("闂佸搫鐗滄禍鐐烘偂閿熺姴绠柍褜鍓熼幊? %s\\n闁哄鐗婇幐鎼佸矗?/skill marketplace 闂佸憡鐟﹂崹褰掔嵁閸ヮ剙鍗抽柡澶嬪焾濡鏌熼崹娑樹壕闂佺厧鍟块～鏇㈠焵椤戣法绠皀", name)': 'repl.PrintSafe("\\n未找到技能 %s。您可以使用 /skill marketplace 浏览可用技能，或自建技能。\\n", name)',
    'fmt.Println("濠电偛澶囬崜婵嗭耿娴ｅ壊鍟呴棅顐幘缁犱粙鎮楀☉娆樼劷婵炲牊鍨剁€电厧颤濆畷鍥ㄢ枔")': 'fmt.Println("没有找到任何会话记录。")',
}

fix_file('cmd/agentgo/main.go', replacements)
