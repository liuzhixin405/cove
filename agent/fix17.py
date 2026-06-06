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
    'repl.PrintAbove(fmt.Sprintf("\\n  %s鍏佽锛?(y)纭 / (n)鎷掔粷 / (a)濮嬬粓鍏佽:%s", repl.Yellow, repl.Reset))': 'repl.PrintAbove(fmt.Sprintf("\\n  %s允许？(y)确认 / (n)拒绝 / (a)始终允许:%s", repl.Yellow, repl.Reset))',
    'fmt.Println("濠电偛澶囬崜婵嗭耿娴ｅ壊鍟呴棅顐幘缁犱粙鎮楀☉娆樼劷婵炲牊鍨剁€电厧颤濆畷鍥ㄢ枔")': 'fmt.Println("未找到历史记录。")',
    'fmt.Println("阎熸粎澧楅幐鍛婃櫠阎樺樊鍟呴柛娆忣槹缁犳帒霉阎樹警鍤欏┑顔惧枛瀹旷兘濡搁妸褏顔掗柣鐐寸☉阎妲愬┑鍫㈢＜鐟滃繘鎮界紒妯荤秶闁规儳鍟垮鎶芥煕閹邦剚鍣规い鏃€鍔栫€电厧颤濋鐐电懇闂傚倸鍟伴崰搴ㄥ春瀹€鍐︿汗闁规儳鍟块·鍛归悩渚殭濠殿唤鍠栧畷銉︽偿閵忕姭鎸冮柣鐐寸☉鐞氼偊鍩€")': 'fmt.Println("当前任务正在运行中，请输入继续。")'
}

fix_file('cmd/agentgo/main.go', replacements)
