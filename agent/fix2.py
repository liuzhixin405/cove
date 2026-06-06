import sys
with open('cmd/agentgo/main.go', 'r', encoding='utf-8') as f:
    text = f.read()

text = text.replace('fmt.Println(\"閻庣懓鎲¤ぐ鍐╂叏閻愬搫绀傞柕澶嗘櫅濞呫垽鎮归崶銊︾婵¤绠撳畷姘旂€ｎ剛顦繛瀵稿Ь椤曆勬叏閻旇　鍋撻悷鐗堟拱闁搞劍宀稿畷銉︽償濠靛牏鐛ラ梺鐓庮殠娴滄粍鎱ㄩ埡鍐＜鐟滃繘鎮芥繝姘?)', 'fmt.Println(\"[恢复] 任务已排队，即将开始处理。\")')

with open('cmd/agentgo/main.go', 'w', encoding='utf-8') as f:
    f.write(text)
