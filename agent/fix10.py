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
    'repl.PrintAbove(fmt.Sprintf("\\n  %s妫€娴嬪埌绌鸿緭鍏ワ紝璇峰啀娆¤緭鍏?(y/n/a):%s", repl.Yellow, repl.Reset))': 'repl.PrintAbove(fmt.Sprintf("\\n  %s检测到空输入，请重新输入 (y/n/a):%s", repl.Yellow, repl.Reset))',
    'repl.PrintAbove(fmt.Sprintf("  %s(闂佸搫鐗滄禍锝囨椤撱垹绀傞柕澶樺灣缁€澶婎潡濞戞瑯鐒炬い鎾愁煼楠炲繘骞掗弮鍌氬椽)%s\\n", repl.Dim, repl.Reset))': 'repl.PrintAbove(fmt.Sprintf("  %s(按 y 确认，n 拒绝，a 全部接受)%s\\n", repl.Dim, repl.Reset))',
    'fmt.Fprintf(os.Stderr, "\\n\\x1b[33m闂?闂佸憡鍑归崹鐗堟叏閳哄啯瀚氬┑鐘插暞閻掍粙鏌涘▎鎰仼妤犵偛娲濠氼敋閳ь剟銆?\\x1b[0m\\n")': 'fmt.Fprintf(os.Stderr, "\\n\\x1b[33m⚠ 启动时检测到可能的问题：\\x1b[0m\\n")',
    'repl.PrintSafe("\\r\\n闁荤姴娲╅褑銇愰崶顒佺叆婵炲棙甯╅崵鏍煥濞戞ɑ婀版い鎺撶矒瀹曠兘濡搁埡浣诡仧闂?REPL...\\r\\n")': 'repl.PrintSafe("\\r\\n终端读取错误，正在重新初始化 REPL...\\r\\n")',
    'fmt.Printf("闂傚倸瀚€氼亜鈻庨姀鈥崇窞闁告洦鍘介崐鐐差熆閹壆绨块悷? %v\\n", err)': 'fmt.Printf("警告: 构建用户消息时出错: %v\\n", err)',
    'fmt.Printf("闂佸搫鐗滄禍鐐烘偂閿熺姴宸濋柟瀛樺笚婵? /%s闂侀潧妫楅崐鐣屾椤撱垹绀?/help 闂佸搫琚崕鍐诧耿閸涱垱鏆滄い鏃傚帶琚燶n", name)': 'fmt.Printf("未找到命令 /%s。请使用 /help 查看可用命令。\\n", name)',
    'repl.PrintSafe("\\n%d 婵炴垶鎼╂禍婵囦繆瑜旈幊?\\n", len(prompts))': 'repl.PrintSafe("\\n已安装的技能 (%d):\\n", len(prompts))',
    'repl.PrintSafe("闂佺懓鐏堥崑鎾绘煠鐎圭姵顥夌紒鏂跨埣瀹曢攱娼幍顔炬喒闂佸憡鐟崹鎶藉极? %v\\n", err)': 'repl.PrintSafe("获取技能市场列表失败: %v\\n", err)',
    'repl.PrintSafe("\\n闂佺懓鐏堥崑鎾绘煠鐎圭姵顥夌紒鏂跨埣瀹?(%d 婵炴垶鎼╂禍婊嗐亹閺屻儲鍋?:\\n", len(entries))': 'repl.PrintSafe("\\n技能市场 (%d 个可用技能):\\n", len(entries))',
    'repl.PrintSafe("\\n闁诲海鎳撻ˇ鎶剿? /skill install <闂佸憡鑹剧粔鎯扳叿>\\n")': 'repl.PrintSafe("\\n使用 /skill install <name> 安装技能\\n")',
    'repl.PrintSafe("閻庣懓鎲¤ぐ鍐偩閵娧勫晳? %s 闂?闂備焦褰冪粔鎾箚鎼淬劌瑙﹂幖杈剧稻閺呮悂鏌℃担鍝ュ⒊缂佽鲸绻堥獮瀣冀椤愶絿鍑介梺瑙勪航閸庡娆㈤銏犵?/skill %s\\n", name, name)': 'repl.PrintSafe("成功安装技能 %s！现在可以使用 /skill %s 调用它。\\n", name, name)',
    'repl.PrintSafe("閻庣懓鎲¤ぐ鍐垂閸楃儐鍤? %s\\n缂傚倸鍊归悧鐐垫?~/.agentgo/skills/%s/SKILL.md\\n", name, name)': 'repl.PrintSafe("成功创建本地技能目录 %s，请编辑 ~/.agentgo/skills/%s/SKILL.md\\n", name, name)',
    'repl.PrintSafe("閻庣懓鎲¤ぐ鍐垂閸楃儐鍤? %s 闂?缂傚倸鍊归悧鐐垫?~/.agentgo/skills/%s/SKILL.md\\n", name, name)': 'repl.PrintSafe("成功创建本地技能目录 %s，请编辑 ~/.agentgo/skills/%s/SKILL.md\\n", name, name)',
    'repl.PrintSafe("\\n闂佸搫鐗滄禍鐐烘偂閿熺姴绠柍褜鍓熼幊? %s\\n闁哄鐗婇幐鎼佸矗?/skill marketplace 闂佸憡鐟﹂崹褰掔嵁閸ヮ剙鍗抽柡澶嬪焾濡鏌熼崹娑樹壕闂佺厧鍟块～鏇㈠焵椤戣法绠皀", name)': 'repl.PrintSafe("\\n未找到技能 %s。您可以使用 /skill marketplace 浏览可用技能，或自建技能。\\n", name)',
    'repl.PrintSafe("闂佸搫鐗滄禍鐐烘偂閿熺姴绠柍褜鍓熼幊? %s\\n", name)': 'repl.PrintSafe("未找到配置文件: %s\\n", name)',
    'repl.PrintSafe("闂佹椿娼块崝宥囨兜? /skill create <闂佸憡鑹剧粔鎯扳叿>\\n")': 'repl.PrintSafe("用法: /skill create <name>\\n")',
    'repl.PrintSafe("\\n闂佹椿娼块崝宥夊春濞戞碍瀚氶梺鍨儑濠€?\\n%s\\n", args)': 'repl.PrintSafe("\\n无效的参数: %s\\n", args)',
    'fmt.Println("濠电偛澶囬崜婵嗭耿娴ｅ壊鍟呴棅顐幘缁犱粙鎮楀☉娆樼劷婵炲牊鍨剁€电厧顫濆畷鍥ㄢ枔")': 'fmt.Println("没有找到任何会话记录。")',
    'fmt.Printf("%d 婵炴垶鎼╂禍娆戞閹殿喗瀚?\\n", len(records))': 'fmt.Printf("%d 条会话记录:\\n", len(records))',
}

fix_file('cmd/agentgo/main.go', replacements)
