import re

with open('cmd/agentgo/main.go', 'r', encoding='utf-8') as f:
    text = f.read()

help_text = r'''func printCLIHelp() {
fmt.Println("agentgo 是一款基于 Go 的 AI 终端代理工具。\n" + 
"用法:\n" +
" agentgo                       启动交互式 REPL\n" +
" agentgo -p <prompt>           执行单次询问并输出结果\n" +
" agentgo -p <prompt> --image <path> 执行单次带有图片的询问\n" +
" agentgo -p <prompt> --file <path>  执行单次带有文件的询问\n" +
" agentgo -r <id>               恢复之前的会话记录\n" +
" agentgo --list-sessions       列出所有会话记录\n" +
" agentgo -d                    开启调试模式并打印日志\n" +
" agentgo --doctor              运行环境自检\n" +
" agentgo --config              查看配置文件\n" +
" agentgo -v, --version         输出版本信息\n" +
" agentgo -h, --help            查看帮助信息\n\n" +
"插件与技能指令:\n" +
" /skill [name]               执行某个技能\n" +
" /skill marketplace          查看技能市场\n" +
" /skill install <name>       安装技能\n" +
" /skill create <name>        创建新的本地技能\n\n" +
"REPL 内置命令:\n" +
" /model, /provider, /api-key, /base-url, /mode, /budget\n" +
" /cost, /config, /system, /context, /compact\n" +
" /attach <path...>, /attach list, /attach remove <id>, /attach clear\n" +
" /commit, /review, /diff, /export\n" +
" /resume, /memory, /mcp, /plugin\n" +
" /doctor, /status, /stats, /permissions\n" +
" /cd, /help, /exit")
fmt.Println("\n提示: 在 prompt 中可以使用 @文件路径 的形式来附带文件。例如: 帮我分析这段日志 @logs/app.log")
}'''

text = re.sub(r'func printCLIHelp\(\) \{.*?(?=\n\nfunc truncateDesc)', help_text, text, flags=re.DOTALL)

with open('cmd/agentgo/main.go', 'w', encoding='utf-8') as f:
    f.write(text)
