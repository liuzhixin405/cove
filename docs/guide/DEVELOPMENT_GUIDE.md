# Cove Agent 瀹屾暣寮€鍙戞寚鍗?
> **閫傜敤璇昏€?*: 鏂版墜寮€鍙戣€呫€傛湰鏂囨。灏嗗甫浣犱粠闆跺紑濮嬬悊瑙ｆ暣涓?Cove Agent 椤圭洰鐨勬灦鏋勩€佽璁＄悊蹇靛拰瀹炵幇缁嗚妭锛岃瀹屽嵆鍙笂鎵嬪紑鍙戙€?
---

## 鐩綍

1. [椤圭洰姒傝堪](#1-椤圭洰姒傝堪)
2. [鎶€鏈爤涓庝緷璧朷(#2-鎶€鏈爤涓庝緷璧?
3. [鐩綍缁撴瀯鎬昏](#3-鐩綍缁撴瀯鎬昏)
4. [鏍稿績鏋舵瀯璁捐](#4-鏍稿績鏋舵瀯璁捐)
5. [妯″潡璇﹁В](#5-妯″潡璇﹁В)
   - [5.1 CLI 鍏ュ彛灞俔(#51-cli-鍏ュ彛灞?
   - [5.2 Engine 寮曟搸灞俔(#52-engine-寮曟搸灞?
   - [5.3 API 灞俔(#53-api-灞?
   - [5.4 Tool 宸ュ叿绯荤粺](#54-tool-宸ュ叿绯荤粺)
   - [5.5 Permission 鏉冮檺绯荤粺](#55-permission-鏉冮檺绯荤粺)
   - [5.6 Plan 璁″垝绯荤粺](#56-plan-璁″垝绯荤粺)
   - [5.7 Memory 璁板繂绯荤粺](#57-memory-璁板繂绯荤粺)
   - [5.8 Skills 鎶€鑳界郴缁焆(#58-skills-鎶€鑳界郴缁?
   - [5.9 Session 浼氳瘽绠＄悊](#59-session-浼氳瘽绠＄悊)
   - [5.10 MCP 闆嗘垚](#510-mcp-闆嗘垚)
   - [5.11 Browser 娴忚鍣╙(#511-browser-娴忚鍣?
   - [5.12 REPL 浜や簰灞俔(#512-repl-浜や簰灞?
   - [5.13 Context 涓婁笅鏂囩鐞哴(#513-context-涓婁笅鏂囩鐞?
   - [5.14 杈呭姪妯″潡](#514-杈呭姪妯″潡)
6. [鏍稿績鏁版嵁娴乚(#6-鏍稿績鏁版嵁娴?
7. [鍏抽敭璁捐妯″紡](#7-鍏抽敭璁捐妯″紡)
8. [濡備綍杩愯涓庢瀯寤篯(#8-濡備綍杩愯涓庢瀯寤?
9. [濡備綍鎵╁睍绯荤粺](#9-濡備綍鎵╁睍绯荤粺)
10. [娴嬭瘯绛栫暐](#10-娴嬭瘯绛栫暐)

---

## 1. 椤圭洰姒傝堪

Cove 鏄竴涓敤 **Go 璇█** 缂栧啓鐨?**AI 缂栫▼鍔╂墜锛圕oding Agent锛?*锛岃繍琛屽湪缁堢锛圕LI锛変腑銆傚畠鐨勬牳蹇冭兘鍔涙槸锛?
- 涓?LLM锛堝ぇ璇█妯″瀷锛夊璇濓紝鎺ユ敹鐢ㄦ埛鐨勮嚜鐒惰瑷€缂栫▼浠诲姟
- 璋冪敤 **宸ュ叿锛圱ools锛?* 鏉ヨ鍐欐枃浠躲€佹墽琛屽懡浠ゃ€佹悳绱唬鐮佺瓑
- 閫氳繃 **璁″垝锛圥lan锛?* 绯荤粺灏嗗鏉備换鍔℃媶瑙ｄ负鍙墽琛屾楠?- 閫氳繃 **鏉冮檺锛圥ermission锛?* 绯荤粺瀹夊叏鍦版帶鍒跺嵄闄╂搷浣?- 閫氳繃 **璁板繂锛圡emory锛?* 鍜?**鎶€鑳斤紙Skills锛?* 绯荤粺浠庡璇濅腑瀛︿範
- 鏀寔 **MCP 鍗忚** 鎵╁睍澶栭儴宸ュ叿
- 鏀寔 **澶?Agent 鍗忎綔**锛堝瓙浠ｇ悊銆佸洟闃燂級

**鏍稿績璁捐鐞嗗康**: 鍗曡繘绋嬨€佷簨浠堕┍鍔ㄣ€佹潈闄愬彲鎺с€佹笎杩涘紡瀛︿範銆?
---

## 2. 鎶€鏈爤涓庝緷璧?
```
璇█:          Go 1.22+
HTTP 瀹㈡埛绔?   net/http锛堟爣鍑嗗簱锛?缁堢 UI:       ANSI 杞箟搴忓垪 + 鑷畾涔?readline
鏁版嵁瀛樺偍:      鍐呭瓨 + JSON 鏂囦欢鎸佷箙鍖栵紙鏈潵鍙兘鎵╁睍 SQLite锛?AI API:        Anthropic API锛堜富锛? OpenAI 鍏煎 API
娴嬭瘯:          Go 鏍囧噯 testing 鍖?```

**鏍稿績渚濊禆锛堟潵鑷?go.mod锛?*:
- `github.com/liuzhixin405/cove` 鈥?妯″潡鏍硅矾寰?- Go 鏍囧噯搴撲负涓伙紝澶栭儴渚濊禆鏋佸皯锛堣璁″師鍒欙細鏈€灏忎緷璧栵級

---

## 3. 鐩綍缁撴瀯鎬昏

```
cove/agent/
鈹溾攢鈹€ go.mod                          # Go 妯″潡瀹氫箟
鈹溾攢鈹€ go.sum                          # 渚濊禆鏍￠獙
鈹溾攢鈹€ README.md                       # 椤圭洰璇存槑
鈹溾攢鈹€ CHANGELOG.md                    # 鐗堟湰鍙樻洿鏃ュ織
鈹溾攢鈹€ DEVELOPMENT_GUIDE.md            # 鏈枃妗?鈹?鈹溾攢鈹€ cli/                            # 鍛戒护琛屽叆鍙?鈹?  鈹斺攢鈹€ cove/
鈹?      鈹溾攢鈹€ main.go                 # 绋嬪簭鍏ュ彛锛?49琛岋紝鏍稿績鍚姩閫昏緫锛?鈹?      鈹溾攢鈹€ app_bootstrap.go        # 搴旂敤鍚姩寮曞锛?96琛岋級
鈹?      鈹溾攢鈹€ repl_tui.go             # runTUI() + useTUI() + 闃熷垪妗ユ帴锛?53琛岋級
鈹?      鈹溾攢鈹€ repl_loop.go            # runREPL() fallback 妯″紡锛?23琛岋級
鈹?      鈹溾攢鈹€ chat_interaction.go     # 鍗曟浜や簰澶勭悊锛?51琛岋級
鈹?      鈹斺攢鈹€ registry.go             # 宸ュ叿娉ㄥ唽锛?02琛岋級
鈹?鈹溾攢鈹€ internal/                       # 鍐呴儴鍖咃紙鏍稿績瀹炵幇锛?鈹?  鈹溾攢鈹€ tui/                        # 鈽?鍏ㄥ睆 TUI锛堝綋鍓嶉粯璁や氦浜掓ā寮忥級
鈹?  鈹?  鈹溾攢鈹€ tui.go                  # Bubble Tea 妯″瀷锛?14琛岋級
鈹?  鈹?  鈹溾攢鈹€ app.go                  # 绋嬪簭鍖呰鍣?+ Bridge Helpers锛?9琛岋級
鈹?  鈹?  鈹斺攢鈹€ styles.go               # 鏍峰紡 & 甯冨眬娓叉煋锛?63琛岋級
鈹?  鈹?  鈹斺攢鈹€ theme/                  # 主题系统（5套内置主题，20+语义化颜色令牌）
鈹?  鈹?鈹?  鈹溾攢鈹€ api/                        # AI API 鎶借薄灞?鈹?  鈹?  鈹溾攢鈹€ provider.go             # 缁熶竴 Provider 鎺ュ彛
鈹?  鈹?  鈹溾攢鈹€ provider_catalog.go     # 鍐呯疆 Provider 鐩綍
鈹?  鈹?  鈹溾攢鈹€ anthropic.go            # Anthropic API 瀹炵幇
鈹?  鈹?  鈹溾攢鈹€ openai_compat.go        # OpenAI 鍏煎 API 瀹炵幇
鈹?  鈹?  鈹溾攢鈹€ keypool.go              # API Key 姹狅紙杞浆銆佹晠闅滆浆绉伙級
鈹?  鈹?  鈹溾攢鈹€ ratelimit.go            # 閫熺巼闄愬埗杩借釜
鈹?  鈹?  鈹溾攢鈹€ retry.go                # 鎸囨暟閫€閬块噸璇?鈹?  鈹?  鈹斺攢鈹€ prompt_cache.go         # Prompt 缂撳瓨绛栫暐
鈹?  鈹?鈹?  鈹溾攢鈹€ engine/                     # 鏍稿績寮曟搸
鈹?  鈹?  鈹溾攢鈹€ engine.go               # 寮曟搸涓婚€昏緫锛?518琛岋級
鈹?  鈹?  鈹溾攢鈹€ activity.go             # 娲诲姩杩借釜 & 鍗￠】妫€娴?鈹?  鈹?  鈹溾攢鈹€ review.go               # 鍚庡彴瀵硅瘽鍥為【 & 鑷姩瀛︿範
鈹?  鈹?  鈹斺攢鈹€ engine_test.go          # 寮曟搸娴嬭瘯锛?076琛岋級
鈹?  鈹?鈹?  鈹溾攢鈹€ tool/                       # 宸ュ叿绯荤粺
鈹?  鈹?  鈹溾攢鈹€ tool.go                 # 宸ュ叿鎺ュ彛瀹氫箟
鈹?  鈹?  鈹溾攢鈹€ registry.go             # 宸ュ叿娉ㄥ唽琛?鈹?  鈹?  鈹溾攢鈹€ bash.go                 # Shell 鍛戒护鎵ц
鈹?  鈹?  鈹溾攢鈹€ read.go                 # 鏂囦欢璇诲彇
鈹?  鈹?  鈹溾攢鈹€ write.go                # 鏂囦欢鍐欏叆
鈹?  鈹?  鈹溾攢鈹€ edit.go                 # 鏂囦欢绮剧‘缂栬緫
鈹?  鈹?  鈹溾攢鈹€ grep.go                 # 鍐呭鎼滅储锛坮ipgrep锛?鈹?  鈹?  鈹溾攢鈹€ glob.go                 # 鏂囦欢鍚嶅尮閰嶆悳绱?鈹?  鈹?  鈹溾攢鈹€ webfetch.go             # 缃戦〉鎶撳彇
鈹?  鈹?  鈹溾攢鈹€ powershell.go           # PowerShell 鎵ц锛圵indows锛?鈹?  鈹?  鈹溾攢鈹€ advanced_tools_task_core.go    # 楂樼骇宸ュ叿锛氫换鍔°€丄gent 瀛愯繘绋?鈹?  鈹?  鈹溾攢鈹€ advanced_tools_agent_skill.go  # 楂樼骇宸ュ叿锛氭妧鑳借皟鐢?鈹?  鈹?  鈹斺攢鈹€ advanced_tools_plan_worktree.go # 楂樼骇宸ュ叿锛氳鍒掋€佸伐浣滄爲
鈹?  鈹?鈹?  鈹溾攢鈹€ plan/                       # 璁″垝绯荤粺
鈹?  鈹?  鈹溾攢鈹€ plan.go                 # 璁″垝鏁版嵁缁撴瀯 & 瑙ｆ瀽
鈹?  鈹?  鈹斺攢鈹€ executor.go             # 璁″垝鎵ц鍣?鈹?  鈹?鈹?  鈹溾攢鈹€ permission/                 # 鏉冮檺绯荤粺
鈹?  鈹?  鈹溾攢鈹€ permission.go           # 鏉冮檺妯″紡 & 鍐崇瓥
鈹?  鈹?  鈹斺攢鈹€ classifier.go           # 宸ュ叿鍒嗙被鍣紙鑷姩鍒ゆ柇鍗遍櫓绛夌骇锛?鈹?  鈹?鈹?  鈹溾攢鈹€ session/                    # 浼氳瘽鎸佷箙鍖?鈹?  鈹?  鈹溾攢鈹€ store.go                # 浼氳瘽瀛樺偍锛堟枃浠?+ JSON锛?鈹?  鈹?  鈹斺攢鈹€ store_test.go           # 瀛樺偍娴嬭瘯
鈹?  鈹?鈹?  鈹溾攢鈹€ memory/                     # 璁板繂绯荤粺
鈹?  鈹?  鈹溾攢鈹€ store.go                # 璁板繂瀛樺偍锛圔M25 + 鍚戦噺锛?鈹?  鈹?  鈹溾攢鈹€ bm25.go                 # BM25 鍏抽敭璇嶆绱?鈹?  鈹?  鈹斺攢鈹€ embed.go                # 浼祵鍏?& 鍚戦噺瀛樺偍
鈹?  鈹?鈹?  鈹溾攢鈹€ skills/                     # 鎶€鑳界郴缁?鈹?  鈹?  鈹溾攢鈹€ skills.go               # 鎶€鑳芥敞鍐?& 鍔犺浇
鈹?  鈹?  鈹斺攢鈹€ skills_test.go          # 鎶€鑳芥祴璇?鈹?  鈹?鈹?  鈹溾攢鈹€ mcp/                        # MCP 鍗忚闆嗘垚
鈹?  鈹?  鈹溾攢鈹€ pool.go                 # MCP 杩炴帴姹?鈹?  鈹?  鈹斺攢鈹€ client.go               # MCP 瀹㈡埛绔?鈹?  鈹?鈹?  鈹溾攢鈹€ browser/                    # 缃戦〉娴忚鍣?鈹?  鈹?  鈹斺攢鈹€ browser.go              # HTTP 鎶撳彇 + HTML鈫掓枃鏈浆鎹?鈹?  鈹?鈹?  鈹溾攢鈹€ repl/                       # 缁堢浜や簰
鈹?  鈹?  鈹溾攢鈹€ color.go                # ANSI 棰滆壊宸ュ叿
鈹?  鈹?  鈹斺攢鈹€ readline.go             # 鑷畾涔夎缂栬緫鍣?鈹?  鈹?鈹?  鈹溾攢鈹€ context/                    # 椤圭洰涓婁笅鏂?鈹?  鈹?  鈹斺攢鈹€ context.go              # 椤圭洰鏂囦欢鍒嗘瀽
鈹?  鈹?鈹?  鈹溾攢鈹€ repomap/                    # 浠撳簱鍦板浘
鈹?  鈹?  鈹斺攢鈹€ repomap.go              # 浠ｇ爜搴撶粨鏋勫垎鏋?鈹?  鈹?鈹?  鈹溾攢鈹€ config/                     # 閰嶇疆绠＄悊
鈹?  鈹?  鈹斺攢鈹€ config.go               # 閰嶇疆鍔犺浇 & 楠岃瘉
鈹?  鈹?鈹?  鈹溾攢鈹€ log/                        # 鏃ュ織绯荤粺
鈹?  鈹?  鈹斺攢鈹€ logger.go               # 鍒嗙骇鏃ュ織 + 閿欒姹囨祦
鈹?  鈹?鈹?  鈹溾攢鈹€ cost/                       # 鎴愭湰杩借釜
鈹?  鈹?  鈹斺攢鈹€ tracker.go              # Token 璐圭敤璁＄畻
鈹?  鈹?鈹?  鈹溾攢鈹€ token/                      # Token 浼扮畻
鈹?  鈹?  鈹斺攢鈹€ token.go                # 绠€鍗?Token 璁℃暟
鈹?  鈹?鈹?  鈹溾攢鈹€ diagnostic/                 # 璇婃柇绯荤粺
鈹?  鈹?  鈹溾攢鈹€ checker.go              # 绯荤粺鍋ュ悍妫€鏌?鈹?  鈹?  鈹溾攢鈹€ recorder.go             # 杩愯鏃朵簨浠惰褰?鈹?  鈹?  鈹溾攢鈹€ errors.go               # 閿欒瀹氫箟
鈹?  鈹?  鈹斺攢鈹€ diagnostic_test.go      # 璇婃柇娴嬭瘯
鈹?  鈹?鈹?  鈹溾攢鈹€ checkpoint/                 # 妫€鏌ョ偣绯荤粺
鈹?  鈹?  鈹斺攢鈹€ checkpoint.go           # 鏂囦欢淇敼鍓嶈嚜鍔ㄥ浠?鈹?  鈹?鈹?  鈹溾攢鈹€ notes/                      # 绗旇绯荤粺
鈹?  鈹?  鈹斺攢鈹€ notes.go                # 浼氳瘽绗旇绠＄悊
鈹?  鈹?鈹?  鈹溾攢鈹€ onboarding/                 # 鏂版墜寮曞
鈹?  鈹?  鈹斺攢鈹€ onboarding.go           # 棣栨浣跨敤寮曞娴佺▼
鈹?  鈹?鈹?  鈹溾攢鈹€ state/                      # 鐘舵€佸畾涔?鈹?  鈹?  鈹斺攢鈹€ state.go                # 搴旂敤鐘舵€佹灇涓?鈹?  鈹?鈹?  鈹溾攢鈹€ plugin/                     # 鎻掍欢绯荤粺
鈹?  鈹?  鈹斺攢鈹€ plugin.go               # 鎻掍欢鍔犺浇 & 绠＄悊
鈹?  鈹?鈹?  鈹溾攢鈹€ hooks/                      # 閽╁瓙绯荤粺
鈹?  鈹?  鈹斺攢鈹€ hooks.go                # 鐢熷懡鍛ㄦ湡閽╁瓙
鈹?  鈹?鈹?  鈹溾攢鈹€ guardrail/                  # 瀹夊叏鎶ゆ爮
鈹?  鈹?  鈹斺攢鈹€ guardrail.go            # 杈撳叆/杈撳嚭瀹夊叏妫€鏌?鈹?  鈹?鈹?  鈹溾攢鈹€ delegate/                   # 浠ｇ悊鏈哄埗
鈹?  鈹?  鈹斺攢鈹€ delegate.go             # 浠诲姟濮旀墭
鈹?  鈹?鈹?  鈹溾攢鈹€ extract/                    # 鍐呭鎻愬彇
鈹?  鈹?  鈹斺攢鈹€ extract.go              # 浠庡璇濅腑鎻愬彇缁撴瀯鍖栨暟鎹?鈹?  鈹?鈹?  鈹斺攢鈹€ dream/                      # 姊︽兂锛堝弽鎬濓級绯荤粺
鈹?      鈹斺攢鈹€ dream.go                # Agent 鑷垜鍙嶆€?鈹?鈹溾攢鈹€ testdata/                       # 娴嬭瘯鏁版嵁
鈹?  鈹斺攢鈹€ ...
鈹?鈹斺攢鈹€ scripts/                        # 杈呭姪鑴氭湰
    鈹斺攢鈹€ ...
```

---

## 4. 鏍稿績鏋舵瀯璁捐

### 4.1 鏁翠綋鏋舵瀯鍥?
```
鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?                   CLI Entry (main.go)                    鈹?鈹? - 瑙ｆ瀽鍙傛暟銆佸姞杞介厤缃€佸垵濮嬪寲鎵€鏈夊瓙绯荤粺                      鈹?鈹? - 璋冪敤 useTUI() 鍒ゆ柇浜や簰妯″紡                              鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?        鈹?TUI (榛樿)                     鈹?REPL (fallback)
        鈻?                              鈻?鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?  鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?runTUI()          鈹?  鈹?runREPL()                       鈹?鈹?(repl_tui.go)     鈹?  鈹?(repl_loop.go)                  鈹?鈹?- Bubble Tea 鍏ㄥ睆 鈹?  鈹?- 琛屽紡 ANSI 杞箟搴忓垪             鈹?鈹?- 缁撴瀯鍖?turn     鈹?  鈹?- readline 鑷畾涔夌紪杈戝櫒          鈹?鈹?- 瑕嗙洊灞?榧犳爣     鈹?  鈹?- 浠呯閬?--no-tui/              鈹?鈹?- Bridge Helpers  鈹?  鈹?  COVE_TUI=0 鏃惰Е鍙?            鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?  鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?        鈹?                       鈹?        鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?                 鈹?鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈻尖攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?                 Engine (engine.go)                       鈹?鈹? - 鏍稿績缂栨帓鍣細绠＄悊娑堟伅鍘嗗彶锛岃皟鐢?AI锛屾墽琛屽伐鍏?              鈹?鈹? - 閫氳繃鍥炶皟 onDelta/onReasoning/onEngineOutput 鎺ㄩ€佽緭鍑?   鈹?鈹? - TUI 閫氳繃 App.Send* bridge, REPL 鐩存帴 fmt.Print         鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?        鈹?      鈹?      鈹?      鈹?        鈹?   鈹屸攢鈹€鈹€鈹€鈻尖攢鈹€鈹?鈹屸攢鈻尖攢鈹€鈹?鈹屸攢鈹€鈻尖攢鈹€鈹?鈹屸攢鈻尖攢鈹€鈹€鈹?鈹屸攢鈹€鈹€鈻尖攢鈹€鈹€鈹€鈹€鈹€鈹?   鈹?API   鈹?鈹俆ool鈹?鈹侾lan 鈹?鈹侾erm 鈹?鈹?Memory/  鈹?   鈹?Layer 鈹?鈹係ys 鈹?鈹侲xec 鈹?鈹侰heck鈹?鈹?Skills   鈹?   鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹斺攢鈹€鈹€鈹€鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?```

### 4.2 鏍稿績璁捐鍘熷垯

1. **鍗曚竴鍏ュ彛**: 鏁翠釜绋嬪簭鍙湁涓€涓?`main.go`锛屾墍鏈夊垵濮嬪寲鍦?`app_bootstrap.go` 涓畬鎴?2. **鎺ュ彛鎶借薄**: API Provider銆乀ool銆丮emory 绛夋牳蹇冪粍浠堕兘瀹氫箟浜嗘帴鍙ｏ紝渚夸簬鎵╁睍
3. **鍏虫敞鍒嗙**: 姣忎釜 `internal/` 瀛愬寘鑱岃矗鍗曚竴锛岄€氳繃 Engine 鍗忚皟
4. **瀹夊叏绗竴**: 鏉冮檺绯荤粺瀵规墍鏈夊啓鎿嶄綔杩涜鎷︽埅锛岄粯璁ら渶瑕佺敤鎴风‘璁?5. **浼橀泤闄嶇骇**: 褰撴煇涓瓙绯荤粺涓嶅彲鐢ㄦ椂锛堝 MCP 鏈嶅姟鍣ㄦ湭鍚姩锛夛紝涓嶅奖鍝嶆牳蹇冨姛鑳?
---

## 5. 妯″潡璇﹁В

### 5.1 CLI 鍏ュ彛灞?
#### 5.1.1 main.go - 绋嬪簭鍏ュ彛

**鏂囦欢**: `cli/cove/main.go`锛?49 琛岋級

**鏍稿績娴佺▼**:

```go
func main() {
    // 1. 瑙ｆ瀽鍛戒护琛屽弬鏁?    // 2. 鍔犺浇閰嶇疆鏂囦欢锛垀/.cove/config.json 鎴栫幆澧冨彉閲忥級
    // 3. 鍒涘缓 API Provider锛圓nthropic 鎴?OpenAI 鍏煎锛?    // 4. 鍒濆鍖?Engine锛堝紩鎿庢槸鏍稿績缂栨帓鍣級
    // 5. 娉ㄥ唽鍐呯疆宸ュ叿锛坆ash, read, write, edit, grep, glob, webfetch, ...锛?    // 6. 鍒濆鍖?MCP 杩炴帴姹狅紙濡傛灉鏈夐厤缃?MCP 鏈嶅姟鍣級
    // 7. 鍔犺浇 Memory銆丼kills銆丼ession
    // 8. 璁剧疆鏉冮檺妯″紡
    // 9. 鍚姩 REPL 浜や簰寰幆
    // 10. 搴旂敤閫€鍑烘椂淇濆瓨鐘舵€?}
```

**鍏抽敭鍚姩姝ラ**:

1. **閰嶇疆瑙ｆ瀽**: 浠?`~/.cove/config.json` 璇诲彇锛屼紭鍏堢骇锛氬懡浠よ鍙傛暟 > 鐜鍙橀噺 > 閰嶇疆鏂囦欢 > 榛樿鍊?2. **Provider 鍒涘缓**: 鏍规嵁 `provider.name` 閫夋嫨 Anthropic 鎴?OpenAI 鍏煎锛屾敮鎸佸 Key 杞浆
3. **Engine 鍒濆鍖?*: `engine.New(cfg)` 鍒涘缓鏍稿績寮曟搸瀹炰緥
4. **宸ュ叿娉ㄥ唽**: 閫氳繃 `registry.go` 灏嗘墍鏈夊唴缃伐鍏锋敞鍐屽埌 Engine
5. **MCP 鍚姩**: 寮傛杩炴帴鎵€鏈夐厤缃殑 MCP 鏈嶅姟鍣紝灏嗗叾宸ュ叿娉ㄥ唽鍒?Engine
6. **REPL 鍚姩**: 杩涘叆鏃犻檺寰幆锛岃鐢ㄦ埛杈撳叆 鈫?Engine 澶勭悊 鈫?杈撳嚭鍝嶅簲

#### 5.1.2 app_bootstrap.go - 鍚姩寮曞

**鏂囦欢**: `cli/cove/app_bootstrap.go`锛?96 琛岋級

璐熻矗灏?`main.go` 涓殑鍒濆鍖栭€昏緫妯″潡鍖栵細

- `bootstrapConfig()`: 鍔犺浇骞堕獙璇侀厤缃?- `bootstrapAPI()`: 鍒涘缓 API Provider锛堝惈 Key 姹狅級
- `bootstrapTools()`: 娉ㄥ唽鎵€鏈夊伐鍏?- `bootstrapMCP()`: 鍒濆鍖?MCP 杩炴帴
- `bootstrapMemory()`: 鍔犺浇璁板繂绯荤粺
- `bootstrapSkills()`: 鍔犺浇鎶€鑳界郴缁?
#### 5.1.3 repl_loop.go - REPL 涓诲惊鐜?
**鏂囦欢**: `cli/cove/repl_loop.go`锛?23 琛岋級

**鏍稿績鑱岃矗**:
- 鏄剧ず鎻愮ず绗?`> `
- 鏀寔澶氳杈撳叆锛堜互 `\` 缁撳熬缁锛?- 澶勭悊鐗规畩鍛戒护锛坄/help`, `/exit`, `/clear`, `/undo` 绛夛級
- 璋冪敤 `chat_interaction.go` 澶勭悊鍗曟瀵硅瘽
- 绠＄悊涓婁笅鏂囩獥鍙ｏ紙鑷姩鍘嬬缉杩囬暱鍘嗗彶锛?- 澶勭悊 Ctrl+C 涓柇

**鍏抽敭甯搁噺**:
```go
const maxContextMessages = 200  // 鏈€澶ф秷鎭暟
const maxContextTokens  = 90000 // 鏈€澶?Token 鏁?```

**涓婁笅鏂囩獥鍙ｇ鐞嗙瓥鐣?*:
褰撴秷鎭巻鍙茶秴杩囬檺鍒舵椂锛屼繚鐣?`system` 娑堟伅 + 鏈€鏃╃殑 5 鏉?+ 鏈€杩戠殑 N 鏉★紝涓棿閮ㄥ垎鍘嬬缉涓烘憳瑕併€?
#### 5.1.4 chat_interaction.go - 鍗曟浜や簰

**鏂囦欢**: `cli/cove/chat_interaction.go`锛?51 琛岋級

灏佽鍗曟鐢ㄦ埛娑堟伅鐨勫鐞嗘祦绋嬶細

```go
func chatInteraction(eng *engine.Engine, userInput string, ...) {
    // 1. 鏋勫缓鐢ㄦ埛娑堟伅
    // 2. 璋冪敤 eng.RunMessageWithStream() 娴佸紡鑾峰彇鍝嶅簲
    // 3. 瀹炴椂杈撳嚭 delta锛圓I 閫愬瓧杈撳嚭鏁堟灉锛?    // 4. 澶勭悊宸ュ叿璋冪敤銆佹潈闄愯姹?    // 5. 杩斿洖鏈€缁堝搷搴旀枃鏈?}
```

---

### 5.2 Engine 寮曟搸灞?
**鏂囦欢**: `internal/engine/engine.go`锛?518 琛岋級

Engine 鏄」鐩渶鏍稿績鐨勬ā鍧楋紝鏄墍鏈夊瓙绯荤粺鐨?*缂栨帓涓績**銆?
#### 5.2.1 Engine 缁撴瀯浣?
```go
type Engine struct {
    config    Config          // 寮曟搸閰嶇疆
    provider  api.Provider    // AI Provider 鎺ュ彛
    messages  []api.Message   // 褰撳墠瀵硅瘽鍘嗗彶
    tools     []tool.Tool     // 宸叉敞鍐屽伐鍏峰垪琛?    toolReg   *tool.Registry   // 宸ュ叿娉ㄥ唽琛?
    // 鏉冮檺
    perm      *permission.Checker
    PermissionPrompt func(toolName string, input map[string]any, reason string) bool

    // 璁″垝绯荤粺
    plan      *plan.Plan
    planExec  *plan.Executor

    // 璁板繂 & 鎶€鑳?    memStore  *memory.Store
    skillMgr  *skills.Manager

    // 浼氳瘽
    sessionStore *session.Store

    // MCP
    mcpPool   *mcp.Pool

    // 鎴愭湰杩借釜
    costTracker *cost.Tracker

    // 娲诲姩鐩戞帶锛堝崱椤挎娴嬶級
    acts      map[uint64]*activity
    actMu     sync.Mutex
    actSeq    uint64
    // ... 鏇村瀛楁
}
```

#### 5.2.2 鏍稿績鏂规硶: RunMessageWithStream

杩欐槸 Engine 鏈€閲嶈鐨勬柟娉曪紝澶勭悊涓€鏉＄敤鎴锋秷鎭殑瀹屾暣鐢熷懡鍛ㄦ湡锛?
```go
func (e *Engine) RunMessageWithStream(
    ctx context.Context,
    msg api.Message,
    onDelta func(string),       // 娴佸紡杈撳嚭鍥炶皟
    interrupt <-chan struct{},    // 涓柇淇″彿
) (reply string, err error)
```

**瀹屾暣澶勭悊娴佺▼**:

```
鐢ㄦ埛娑堟伅
  鈹?  鈻?鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?1. 娣诲姞鐢ㄦ埛娑堟伅鍒?e.messages              鈹?鈹溾攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?2. 娉ㄥ叆绯荤粺鎻愮ず锛圫ystem Prompt锛?         鈹?鈹?   - 瑙掕壊瀹氫箟 + 鍙敤宸ュ叿鍒楄〃               鈹?鈹?   - 璁板繂涓婁笅鏂?+ 鎶€鑳藉垪琛?                鈹?鈹?   - 椤圭洰涓婁笅鏂?+ 浠撳簱鍦板浘                 鈹?鈹溾攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?3. 璋冪敤 Provider.ChatStream()            鈹?鈹?   - 娴佸紡鑾峰彇 AI 鍝嶅簲                      鈹?鈹?   - 閫氳繃 onDelta 鍥炶皟瀹炴椂杈撳嚭             鈹?鈹?   - 鐩戞帶 interrupt 閫氶亾                  鈹?鈹溾攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?4. 瑙ｆ瀽 AI 鍝嶅簲                           鈹?鈹?   鈹溾攢 绾枃鏈搷搴?鈫?鏀堕泦鍒?reply            鈹?鈹?   鈹溾攢 宸ュ叿璋冪敤璇锋眰 鈫?杩涘叆宸ュ叿鎵ц寰幆       鈹?鈹?   鈹斺攢 鍋滄鍘熷洜 鈫?閫€鍑哄惊鐜?                 鈹?鈹溾攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?5. 銆愬伐鍏锋墽琛屽惊鐜€?                       鈹?鈹?   a. 鏉冮檺妫€鏌ワ紙Permission锛?              鈹?鈹?   b. 骞惰鎵ц鎵€鏈夊伐鍏疯皟鐢?                 鈹?鈹?   c. 鏀堕泦缁撴灉锛屾牸寮忓寲鍚庢坊鍔犲埌娑堟伅鍘嗗彶       鈹?鈹?   d. 鍐嶆璋冪敤 Provider锛堢户缁璇濓級         鈹?鈹?   e. 妫€鏌ラ绠椼€佹鏁伴檺鍒?                  鈹?鈹?   f. 寰幆鐩村埌 AI 涓嶅啀璇锋眰宸ュ叿              鈹?鈹溾攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?6. 鍚庡鐞?                                鈹?鈹?   - 鏇存柊鎴愭湰                              鈹?鈹?   - 瑙﹀彂鍚庡彴鍥為【锛坮eview.go锛?            鈹?鈹?   - 淇濆瓨浼氳瘽                              鈹?鈹?   - 杩斿洖鏈€缁堝搷搴?                         鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?```

#### 5.2.3 绯荤粺鎻愮ず鏋勫缓

Engine 鍦ㄦ瘡娆¤皟鐢?AI 鍓嶆瀯寤虹郴缁熸彁绀猴紙System Prompt锛夛紝鍖呭惈锛?
```
浣犳槸 Cove锛屼竴涓?AI 缂栫▼鍔╂墜銆?浣犲彲浠ヤ娇鐢ㄤ互涓嬪伐鍏凤細
  - bash: 鎵ц Shell 鍛戒护
  - read: 璇诲彇鏂囦欢
  - write: 鍐欏叆鏂囦欢
  - ...锛堝伐鍏峰垪琛級

褰撳墠椤圭洰涓婁笅鏂囷細[椤圭洰鏂囦欢鏍戞憳瑕乚
鐩稿叧璁板繂锛歔浠?Memory 妫€绱㈢殑鐩稿叧璁板繂]
鍙敤鎶€鑳斤細[宸叉敞鍐屾妧鑳藉垪琛╙
瀹夊叏瑙勫垯锛歔鎶ゆ爮瑙勫垯]
```

#### 5.2.4 娲诲姩鐩戞帶锛坅ctivity.go锛?
`activity.go` 瀹炵幇浜?*鍗￠】妫€娴?*鏈哄埗锛?
- 姣忎釜鎿嶄綔闃舵锛圓PI 璋冪敤銆佸伐鍏锋墽琛岋級閮借娉ㄥ唽涓轰竴涓?`activity`
- 鍚庡彴 goroutine 姣?5 绉掓壂鎻忔墍鏈夋椿鍔?- 濡傛灉鏌愪釜娲诲姩 **30 绉掓棤杩涘睍**锛岃緭鍑洪粍鑹茶鍛?`鈿?浠嶅湪銆寈x銆嶏紝宸?xx 鏃犳柊杩涘睍`
- 鐢ㄦ埛鐪嬪埌鍚庡彲浠ユ寜 Ctrl+C 涓柇

#### 5.2.5 鍚庡彴鍥為【锛坮eview.go锛?
姣忔瀵硅瘽鍥炲悎缁撴潫鍚庤嚜鍔ㄨЕ鍙戯紙寮傛锛?0 绉掕秴鏃讹級锛?
1. 鎴彇鏈€杩?10 鏉℃秷鎭綔涓哄揩鐓?2. 鍙戦€佺粰 AI 鍒嗘瀽锛?鏄惁鏈夊€煎緱璁颁綇鐨勫唴瀹癸紵"
3. AI 杩斿洖 `MEMORY: xxx` 鈫?瀛樺叆 Memory
4. AI 杩斿洖 `SKILL: name | steps` 鈫?娉ㄥ唽鏂版妧鑳?5. 鐢ㄦ埛鐪嬪埌 `馃 璁颁綇浜? xxx` 鎴?`馃摎 瀛︿細浜? xxx`

---

### 5.3 API 灞?
#### 5.3.1 Provider 鎺ュ彛锛坧rovider.go锛?
```go
type Provider interface {
    Name() string
    DisplayName() string
    Validate() error
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
    ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error)
}

type ChatRequest struct {
    Model      string
    SystemBase string    // 绯荤粺鎻愮ず
    Messages   []Message // 瀵硅瘽鍘嗗彶
    Tools      []ToolDef // 鍙敤宸ュ叿瀹氫箟
    MaxTokens  int
    Temperature float64
}

type ChatResponse struct {
    Content    string
    ToolCalls  []ToolCall
    StopReason string
    Usage      Usage
}

type Message struct {
    Role         string    // "system" | "user" | "assistant" | "tool"
    Content      string
    ToolCalls    []ToolCall
    ToolCallID   string
    CacheControl string    // Anthropic prompt cache
}
```

#### 5.3.2 Anthropic 瀹炵幇锛坅nthropic.go锛?
**鏂囦欢**: `internal/api/anthropic.go`锛?73 琛岋級

瀹炵幇 Anthropic Messages API 鐨勮皟鐢細

- **绔偣**: `https://api.anthropic.com/v1/messages`
- **璁よ瘉**: `x-api-key` 澶?+ Anthropic 鐗堟湰澶?- **娴佸紡**: Server-Sent Events (SSE) 瑙ｆ瀽
- **宸ュ叿**: 灏?ToolDef 杞崲涓?Anthropic `tool_use` 鏍煎紡
- **缂撳瓨**: 鏀寔 `ephemeral` cache_control
- **閫熺巼闄愬埗**: 瑙ｆ瀽 `anthropic-ratelimit-*` 鍝嶅簲澶?
**鍏抽敭澶勭悊**:
- 娑堟伅鏍煎紡杞崲锛欳ove 鍐呴儴鏍煎紡 鈫?Anthropic API 鏍煎紡
- 娴佸紡浜嬩欢瑙ｆ瀽锛歚content_block_start/delta/stop`
- 閿欒澶勭悊锛氬尯鍒?4xx/5xx锛岄噸璇曠瓥鐣ョ敱 retry.go 澶勭悊

#### 5.3.3 OpenAI 鍏煎瀹炵幇锛坥penai_compat.go锛?
**鏂囦欢**: `internal/api/openai_compat.go`锛?68 琛岋級

鏀寔鎵€鏈?OpenAI Chat Completions API 鍏煎鐨?Provider锛?
- **绔偣**: `{base_url}/v1/chat/completions`
- **璁よ瘉**: `Authorization: Bearer {key}`
- **娴佸紡**: SSE `data: [DONE]`
- **宸ュ叿**: 杞崲涓?OpenAI `function_call` 鏍煎紡

鏀寔鐨勭幆澧冨彉閲忛厤缃細
- `OPENAI_API_KEY`
- `OPENAI_BASE_URL`锛堣嚜瀹氫箟绔偣锛屽 Azure銆佹湰鍦版ā鍨嬶級
- `OPENAI_MODEL`锛堥粯璁?gpt-4o锛?
#### 5.3.4 API Key 姹狅紙keypool.go锛?
**鏂囦欢**: `internal/api/keypool.go`锛?72 琛岋級

绠＄悊澶氫釜 API Key锛屽疄鐜拌嚜鍔ㄦ晠闅滆浆绉伙細

```go
type KeyPool struct {
    keys    []*PoolKey      // Key 鍒楄〃
    current int             // 褰撳墠杞浆浣嶇疆
}

type PoolKey struct {
    Key       string         // API Key
    Status    KeyStatus      // OK / Exhausted / Dead
    CoolUntil time.Time      // 鍐峰嵈鍒颁綍鏃?}
```

**鐘舵€佹満**:
- `KeyOK` 鈫?鍙敤
- `KeyExhausted` 鈫?琚檺娴侊紝鍐峰嵈鍚庡彲鎭㈠
- `KeyDead` 鈫?璁よ瘉澶辫触锛屾案涔呬笉鍙敤

**杞浆绛栫暐**: Round-robin锛岃烦杩?Exhausted锛堥櫎闈炲叏閮ㄤ笉鍙敤锛?
#### 5.3.5 閲嶈瘯鏈哄埗锛坮etry.go锛?
**鏂囦欢**: `internal/api/retry.go`锛?9 琛岋級

鎸囨暟閫€閬块噸璇曪細

```go
func retryWithBackoff[T any](ctx context.Context, cfg retryConfig, operation func() (T, error)) (T, error) {
    for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
        result, err := operation()
        if err == nil { return result, nil }
        if attempt == cfg.MaxRetries || !isRetryable(err) { return zero, err }
        delay := time.Duration(1<<attempt) * cfg.BaseDelay  // 1s, 2s, 4s, 8s...
        // 绛夊緟 delay 鎴?ctx 鍙栨秷
    }
}
```

**鍙噸璇曢敊璇?*: 缃戠粶瓒呮椂銆?xx銆?29锛圧ate Limit锛?**涓嶅彲閲嶈瘯閿欒**: 4xx锛堥櫎 429锛夈€佽璇侀敊璇?
#### 5.3.6 閫熺巼闄愬埗杩借釜锛坮atelimit.go锛?
瑙ｆ瀽 API 鍝嶅簲澶翠腑鐨勯€熺巼闄愬埗淇℃伅锛屽疄鏃舵樉绀猴細

```
璇锋眰:150/200(75%) 閲嶇疆:1m30s | Token:45K/100K(45%) 閲嶇疆:2m
```

---

### 5.4 Tool 宸ュ叿绯荤粺

#### 5.4.1 宸ュ叿鎺ュ彛锛坱ool.go锛?
```go
type Tool interface {
    Def() Def                              // 杩斿洖宸ュ叿瀹氫箟锛堝悕绉般€佹弿杩般€佸弬鏁?Schema锛?    Validate(input Input) string           // 楠岃瘉杈撳叆鍙傛暟锛岃繑鍥為敊璇俊鎭垨绌?    CheckPermissions(input Input, tctx Context) PermissionDecision  // 鏉冮檺妫€鏌?    Call(ctx context.Context, input Input, tctx Context) (Result, error)  // 鎵ц
}

type Def struct {
    Name              string          // 宸ュ叿鍚嶇О锛堝 "bash", "read"锛?    Description       string          // 鎻忚堪锛堢粰 AI 鐪嬬殑锛?    InputSchema       json.RawMessage // JSON Schema 鍙傛暟瀹氫箟
    IsReadOnly        bool            // 鏄惁鍙
    IsConcurrencySafe bool            // 鏄惁骞跺彂瀹夊叏
    UserFacingName    string          // 闈㈠悜鐢ㄦ埛鐨勫悕绉?}
```

#### 5.4.2 鍐呯疆宸ュ叿涓€瑙?
| 宸ュ叿鍚?| 鏂囦欢 | 鍔熻兘 | 鍙 |
|--------|------|------|------|
| `bash` | bash.go | 鎵ц Shell 鍛戒护锛圠inux/macOS锛?| 鉂?|
| `powershell` | powershell.go | 鎵ц PowerShell 鍛戒护锛圵indows锛?| 鉂?|
| `read` | read.go | 璇诲彇鏂囦欢鍐呭 | 鉁?|
| `write` | write.go | 鍐欏叆鏂囦欢锛堣鐩栵級 | 鉂?|
| `edit` | edit.go | 绮剧‘瀛楃涓叉浛鎹㈢紪杈?| 鉂?|
| `grep` | grep.go | 姝ｅ垯鎼滅储锛堝簳灞傝皟鐢?ripgrep锛?| 鉁?|
| `glob` | glob.go | 鏂囦欢鍚?glob 鍖归厤鎼滅储 | 鉁?|
| `webfetch` | webfetch.go | HTTP 鎶撳彇缃戦〉鍐呭 | 鉁?|
| `browser` | webfetch.go | Headless 娴忚鍣ㄦ覆鏌?| 鉁?|
| `question` | advanced_tools_*.go | 鍚戠敤鎴锋彁闂?| 鉁?|
| `todowrite` | advanced_tools_*.go | 鍒涘缓/绠＄悊浠诲姟鍒楄〃 | 鉁?|
| `execute_plan` | advanced_tools_plan_worktree.go | 鎵ц璁″垝浠诲姟 | 鉂?|
| `plan_mode` | advanced_tools_plan_worktree.go | 杩涘叆璁″垝妯″紡 | 鉁?|
| `exit_plan_mode` | advanced_tools_plan_worktree.go | 閫€鍑鸿鍒掓ā寮?| 鉁?|
| `worktree` | advanced_tools_plan_worktree.go | 鍒涘缓 Git 宸ヤ綔鏍?| 鉂?|
| `task` | advanced_tools_task_core.go | 鍒涘缓鍚庡彴浠诲姟 | 鉂?|
| `agent` | advanced_tools_agent_skill.go | 鍚姩瀛?Agent | 鉂?|
| `skill` | advanced_tools_agent_skill.go | 璋冪敤鎶€鑳?| 鉁?|
| `sleep` | advanced_tools_task_core.go | 鏆傚仠绛夊緟 | 鉁?|
| `send_message` | advanced_tools_task_core.go | 鍙戦€佹秷鎭?| 鉁?|

#### 5.4.3 宸ュ叿娉ㄥ唽琛紙registry.go锛?
```go
type Registry struct {
    tools    map[string]Tool
    toolList []Tool       // 淇濇寔鎻掑叆椤哄簭
}

func (r *Registry) Register(t Tool)      // 娉ㄥ唽宸ュ叿
func (r *Registry) Get(name string) Tool // 鎸夊悕鏌ユ壘
func (r *Registry) All() []Tool          // 鎵€鏈夊伐鍏凤紙渚?AI 閫夋嫨锛?func (r *Registry) Defs() []Def          // 鎵€鏈夊伐鍏峰畾涔?```

#### 5.4.4 楂樼骇宸ュ叿璇﹁В

**Task 绯荤粺** (`advanced_tools_task_core.go`):
- `task`: 鍒涘缓鍚庡彴鐙珛浠诲姟锛堝紓姝?goroutine锛?- `task_list`: 鍒楀嚭鎵€鏈夊悗鍙颁换鍔?- `task_update`: 鏇存柊浠诲姟鐘舵€?- `task_stop`: 鍋滄杩愯涓殑浠诲姟

**Agent/Skill 绯荤粺** (`advanced_tools_agent_skill.go`):
- `agent`: 鍒涘缓瀛愪唬鐞嗗鐞嗗鏉傚姝ラ浠诲姟
- `skill`: 鎵ц棰勫畾涔夌殑鎶€鑳藉伐浣滄祦

**Plan/Worktree 绯荤粺** (`advanced_tools_plan_worktree.go`):
- `plan_mode`: 杩涘叆鍙璁″垝妯″紡
- `execute_plan`: 鎵ц璁″垝涓殑鎵€鏈夊緟澶勭悊浠诲姟
- `worktree`: 鍒涘缓 Git 宸ヤ綔鏍戠敤浜庨殧绂讳慨鏀?
#### 5.4.5 宸ュ叿鎵ц鏈哄埗锛圗ngine 涓級

宸ュ叿璋冪敤鍦?Engine 涓槸**骞惰鎵ц**鐨勶細

```go
// engine.go 涓畝鍖栭€昏緫
func (e *Engine) executeToolCalls(toolCalls []api.ToolCall) []tool.Result {
    var wg sync.WaitGroup
    results := make([]tool.Result, len(toolCalls))
    
    for i, tc := range toolCalls {
        wg.Add(1)
        go func(idx int, call api.ToolCall) {
            defer wg.Done()
            defer func() { recover() }()  // panic 鎭㈠
            
            // 1. 鏉冮檺妫€鏌?            // 2. 鍙傛暟楠岃瘉
            // 3. 鎵ц宸ュ叿
            results[idx] = tool.Call(ctx, input, tctx)
        }(i, tc)
    }
    
    wg.Wait()
    return results
}
```

---

### 5.5 Permission 鏉冮檺绯荤粺

#### 5.5.1 鏉冮檺妯″紡锛坧ermission.go锛?
```go
type Mode int
const (
    Default  Mode = iota  // 榛樿锛氬啓鎿嶄綔闇€纭
    Auto                  // 鑷姩锛氬叏閮ㄥ厑璁?    Strict                // 涓ユ牸锛氬叏閮ㄩ渶纭
    Yolo                  // 鏋佸鏉撅細浠呭嵄闄╂搷浣滈渶纭
)
```

#### 5.5.2 鏉冮檺鍐崇瓥

```go
type PermissionDecision struct {
    Decision Decision  // Allow / Deny / Ask
    Reason   string
}
```

#### 5.5.3 宸ュ叿鍒嗙被鍣紙classifier.go锛?
鑷姩灏嗗伐鍏峰垎涓轰笁绫伙細

1. **鍙瀹夊叏**锛坄read`, `grep`, `glob`锛夆啋 姘歌繙鍏佽
2. **浣庨闄╁啓**锛坄write`, `edit`锛夆啋 Default 妯″紡闇€纭
3. **楂橀闄?*锛坄bash`, `powershell`锛夆啋 鏈夊懡浠ゅ垎鏋愯鍒?
鍛戒护鍒嗘瀽鍣ㄦ鏌?Shell 鍛戒护鏄惁鍖呭惈鍗遍櫓鎿嶄綔锛?- `rm -rf /`
- `curl ... | bash`
- 淇敼绯荤粺鏂囦欢
- 缃戠粶鐩戝惉绛?
#### 5.5.4 鏉冮檺閽╁瓙

Engine 鏆撮湶 `PermissionPrompt` 鍑芥暟鎸囬拡锛?
```go
eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
    // 鏄剧ず缁欑敤鎴凤紝璇㈤棶鏄惁鍏佽
    // 杩斿洖 true = 鍏佽锛宖alse = 鎷掔粷
}
```

鍦?CLI 涓紝杩欎釜閽╁瓙浼氾細
1. 鏍煎紡鍖栧伐鍏疯皟鐢ㄤ俊鎭?2. 鏄剧ず `鈿?鍏佽鎵ц bash: "rm file.txt"? [y/N]`
3. 绛夊緟鐢ㄦ埛杈撳叆

---

### 5.6 Plan 璁″垝绯荤粺

#### 5.6.1 Plan 鏁版嵁缁撴瀯锛坧lan.go锛?
```go
type Plan struct {
    ID      string
    Goal    string
    Steps   []Step
    Status  PlanStatus
}

type Step struct {
    ID          string
    Description string
    Status      StepStatus   // pending / in_progress / done / failed
    DependsOn   []string     // 渚濊禆鐨勫叾浠栨楠?ID
    Result      string
}
```

#### 5.6.2 璁″垝鎵ц鍣紙executor.go锛?
```go
type Executor struct {
    plan     *Plan
    engine   *Engine
    maxAgents int            // 鏈€澶у苟鍙戞暟
}

func (e *Executor) Execute(ctx context.Context, parallel bool) error {
    // 1. 鎷撴墤鎺掑簭姝ラ锛堝鐞嗕緷璧栧叧绯伙級
    // 2. 鎸変緷璧栧垎缁勫苟琛屾墽琛?    // 3. 姣忎釜姝ラ鍒涘缓涓€涓瓙 Agent 鎴栫洿鎺ユ墽琛屽伐鍏?    // 4. 鏀堕泦缁撴灉锛屾洿鏂版楠ょ姸鎬?}
```

**骞惰鎵ц绛栫暐**:
- `parallel=true`: 鏃犱緷璧栧叧绯荤殑姝ラ鍚屾椂鎵ц
- `parallel=false`: 椤哄簭鎵ц

---

### 5.7 Memory 璁板繂绯荤粺

#### 5.7.1 璁板繂瀛樺偍锛坰tore.go锛?
```go
type Store struct {
    bm25     *BM25               // 鍏抽敭璇嶆绱?    vecStore *VectorStore        // 鍚戦噺妫€绱?    embedder EmbeddingProvider   // 宓屽叆鎻愪緵鑰?    entries  []MemoryEntry       // 鍏冩暟鎹?}

type MemoryEntry struct {
    ID       int
    Name     string    // "user_preference", "auto", etc.
    Content  string
    Updated  time.Time
}
```

#### 5.7.2 妫€绱㈢瓥鐣?
**娣峰悎妫€绱紙Hybrid Search锛?*:

```go
func (s *Store) Search(query string, topK int) []ScoredDoc {
    // 1. BM25 鍏抽敭璇嶆绱紙蹇€熴€佺簿纭尮閰嶏級
    bm25Results := s.bm25.Search(query, topK*2)
    
    // 2. 鍚戦噺璇箟妫€绱紙璇箟鐩镐技搴︼級
    queryVec := s.embedder.Embed(query)
    vecResults := s.vecStore.Search(queryVec, topK*2)
    
    // 3. 铻嶅悎鎺掑簭锛圔M25 0.7 + 鍚戦噺 0.3锛?    // 4. 鑰冭檻鏃舵晥鎬ц“鍑?    // 5. 杩斿洖 Top-K
}
```

#### 5.7.3 BM25 瀹炵幇锛坆m25.go锛?
缁忓吀鐨?BM25 淇℃伅妫€绱㈢畻娉曪細

- `k1=1.2`: 璇嶉楗卞拰鍙傛暟
- `b=0.75`: 鏂囨。闀垮害褰掍竴鍖栧弬鏁?- 鍒嗚瘝锛氬皬鍐欏寲 + 瀛楁瘝鏁板瓧淇濈暀 + 鍋滅敤璇嶈繃婊?- IDF 璁＄畻锛歚log(1 + (N-df+0.5)/(df+0.5))`

#### 5.7.4 浼祵鍏ワ紙embed.go锛?
涓洪檷浣庢垚鏈紝浣跨敤**瀛楃涓夊厓缁勫搱甯?*鐢熸垚浼祵鍏ュ悜閲忥細

```go
func pseudoEmbedding(text string, dim int) []float32 {
    // 1. 鎻愬彇鎵€鏈夊瓧绗︿笁鍏冪粍锛坱rigram锛?    // 2. 鍝堝笇鍒?[0, dim) 鑼冨洿
    // 3. 绱姞璁℃暟
    // 4. L2 褰掍竴鍖?}
```

杩欐槸涓€涓?economical 鐨勬浛浠ｆ柟妗堬紝閫傜敤浜庤蹇嗘潯鐩?< 1000 鐨勫満鏅€?
---

### 5.8 Skills 鎶€鑳界郴缁?
#### 5.8.1 鎶€鑳藉畾涔夛紙skills.go锛?
```go
type Skill struct {
    Name        string   // 鎶€鑳藉悕绉?    Description string   // 鎻忚堪
    Prompt      string   // 鎶€鑳芥彁绀鸿瘝锛堢粰 AI 鐨勬墽琛屾寚鍗楋級
    Tools       []string // 闇€瑕佺殑宸ュ叿鍒楄〃
}
```

#### 5.8.2 鎶€鑳界鐞嗗櫒

```go
type Manager struct {
    skills      map[string]Skill
    builtinDir  string   // 鍐呯疆鎶€鑳界洰褰?    userDir     string   // 鐢ㄦ埛鑷畾涔夋妧鑳界洰褰?    loader      *Loader  // 鏂囦欢绯荤粺鍔犺浇鍣?}

func (m *Manager) Register(skill Skill)      // 娉ㄥ唽鎶€鑳?func (m *Manager) Execute(name string, args map[string]any) // 鎵ц鎶€鑳?func (m *Manager) ListForAI() string          // 鏍煎紡鍖栫粰 AI 鐪?```

#### 5.8.3 鎶€鑳界洰褰曠粨鏋?
```
skills/
鈹溾攢鈹€ builtin/               # 鍐呯疆鎶€鑳?鈹?  鈹溾攢鈹€ code-review/
鈹?  鈹?  鈹斺攢鈹€ SKILL.md       # 鎶€鑳藉畾涔夋枃浠?鈹?  鈹溾攢鈹€ refactor/
鈹?  鈹?  鈹斺攢鈹€ SKILL.md
鈹?  鈹斺攢鈹€ ...
鈹斺攢鈹€ user/                  # 鐢ㄦ埛鑷畾涔?    鈹斺攢鈹€ my-skill/
        鈹斺攢鈹€ SKILL.md
```

`SKILL.md` 鏍煎紡锛?```markdown
# skill-name
绠€鐭弿杩?
## 鎵ц姝ラ
1. 绗竴姝?..
2. 绗簩姝?..

## 闇€瑕佺殑宸ュ叿
- read
- write
```

---

### 5.9 Session 浼氳瘽绠＄悊

**鏂囦欢**: `internal/session/store.go`锛?84 琛岋級

```go
type Store struct {
    dir     string           // 浼氳瘽鏂囦欢鐩綍
    current *Session
}

type Session struct {
    ID        string
    Created   time.Time
    Updated   time.Time
    Messages  []api.Message   // 瀹屾暣瀵硅瘽鍘嗗彶
    Summary   string          // 浼氳瘽鎽樿
}
```

**鎸佷箙鍖栫瓥鐣?*:
- 瀛樺偍涓?JSON 鏂囦欢锛歚~/.cove/sessions/{id}.json`
- 姣忔瀵硅瘽鍥炲悎鍚庤嚜鍔ㄤ繚瀛?- 鍚姩鏃跺彲鎭㈠涓婃浼氳瘽

---

### 5.10 MCP 闆嗘垚

Model Context Protocol (MCP) 鏄?Anthropic 鎻愬嚭鐨勫紑鏀惧崗璁紝鍏佽澶栭儴宸ュ叿鏈嶅姟鍣ㄦ彁渚涘伐鍏枫€?
#### 5.10.1 MCP 杩炴帴姹狅紙pool.go锛?
```go
type Pool struct {
    servers []*Client         // MCP 瀹㈡埛绔垪琛?    tools   []tool.Tool       // 浠?MCP 鏈嶅姟鍣ㄨ幏鍙栫殑宸ュ叿
}

func NewPool(configs []ServerConfig) *Pool
func (p *Pool) Connect(ctx context.Context) error   // 杩炴帴鎵€鏈夋湇鍔″櫒
func (p *Pool) Tools() []tool.Tool                   // 鑾峰彇鎵€鏈夊伐鍏?func (p *Pool) Close() error
```

#### 5.10.2 MCP 瀹㈡埛绔紙client.go锛?
```go
type Client struct {
    config   ServerConfig
    conn     *stdio.Connection  // 閫氳繃 stdio 涓庡瓙杩涚▼閫氫俊
    tools    []tool.Tool
}

type ServerConfig struct {
    Command string   // 鍚姩鍛戒护锛堝 "npx", "uvx" 绛夛級
    Args    []string // 鍛戒护鍙傛暟
    Env     []string // 鐜鍙橀噺
}
```

**閫氫俊鏈哄埗**:
- 鍚姩瀛愯繘绋嬶紙閫氳繃 `os/exec`锛?- 閫氳繃 stdin/stdout 浼犻€?JSON-RPC 娑堟伅
- `tools/list` 鈫?鑾峰彇宸ュ叿鍒楄〃
- `tools/call` 鈫?璋冪敤宸ュ叿

---

### 5.11 Browser 娴忚鍣?
**鏂囦欢**: `internal/browser/browser.go`锛?42 琛岋級

HTTP 瀹㈡埛绔?+ HTML 杞崲寮曟搸锛?
```go
type Browser struct {
    timeout        time.Duration
    allowLocalhost bool      // 瀹夊叏锛氶粯璁ょ姝㈡湰鍦板湴鍧€
    maxBodySize    int64     // 榛樿 5MB
}
```

**瀹夊叏鎺柦**:
- SSRF 闃叉姢锛氱姝㈣闂唴缃?IP锛?27.0.0.1, 10.x, 192.168.x, 172.16-31.x 绛夛級
- 绂佹璁块棶浜戝厓鏁版嵁绔偣锛坄metadata.google.internal`锛?- 鍝嶅簲澶у皬闄愬埗 5MB
- 杈撳嚭鎴柇 100KB

**HTML 杞崲**:
- `HTMLToText()`: 鍘婚櫎鏍囩锛屼繚鐣欐枃鏈?- `HTMLToMarkdown()`: 杞崲涓?Markdown锛堜繚鐣欐爣棰樸€侀摼鎺ャ€佷唬鐮佸潡銆佸垪琛級

**Headless Chrome 鏀寔**锛堝彲閫夛級:
- 闇€瑕佺紪璇戞爣绛?`-tags chromedp`
- 鎻愪緵 `FetchRendered()` 鍜?`Screenshot()` 鏂规硶
- 榛樿涓嶅彲鐢紝浼橀泤闄嶇骇

---

### 5.12 TUI 鍏ㄥ睆浜や簰灞傦紙鈽?褰撳墠榛樿浜や簰妯″紡锛?
**鏂囦欢**: `internal/tui/tui.go`锛?14琛岋級+ `app.go`锛?9琛岋級+ `styles.go`锛?63琛岋級+ `cli/cove/repl_tui.go`锛?53琛岋紝TUI 鍚姩涓庢ˉ鎺ワ級

#### 5.12.1 璁捐鍔ㄦ満

鏃?REPL 浣跨敤鎵嬪啓 ANSI 杞箟搴忓垪椹卞姩缁堢锛屼緷璧栧師鍦版摝闄?閲嶇粯锛坕n-place erase/redraw锛夈€傝繖绉嶈寮忔ā鍨嬪湪鍚屾椂澶勭悊娴佸紡杈撳嚭銆佸紓姝ヤ换鍔°€佺獥鍙ｅぇ灏忓彉鍖栧拰 Windows 鎺у埗鍙版椂锛?*鏃犳硶鍙潬鍦版敮鎸佸垎鍓插竷灞€**銆?
TUI 鍖呯敤 **鍏ㄥ睆浜ゆ浛灞忓箷 + 鏁村抚閲嶇粯锛圡odel-Update-View锛?* 妯″瀷鏇夸唬浜嗘棫鏂规锛屾瘡甯ч噸鏂拌绠楀畬鏁村竷灞€銆?
**鏍稿績渚濊禆**: Bubble Tea v2锛坄charm.land/bubbletea/v2`锛?+ Lipgloss v2锛坄charm.land/lipgloss/v2`锛?+ Bubbles v2锛坱extarea, textinput, viewport锛?
#### 5.12.2 鍚敤閫昏緫锛坄repl_tui.go`: `useTUI()`锛?
```go
func useTUI() bool {
    if noTUI || os.Getenv("COVE_TUI") == "0" { return false }     // 鏄惧紡绂佺敤 鈫?REPL
    if tuiMode || os.Getenv("COVE_TUI") == "1" { return true }    // 鏄惧紡鍚敤 鈫?TUI
    return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
    // 榛樿锛歴tdin 鍜?stdout 閮芥槸缁堢 鈫?TUI锛涚閬?閲嶅畾鍚?鈫?REPL
}
```

鍛戒护琛屾帶鍒讹細`--tui` / `--no-tui`锛岀幆澧冨彉閲?`COVE_TUI=0/1`

#### 5.12.3 鏍稿績鏁版嵁缁撴瀯

**`turn` 鈥?缁撴瀯鍖栧璇濊疆娆?*锛堟瘡涓洖鍚堜笉鏄墎骞虫枃鏈紝鑰屾槸缁撴瀯鍖栧璞★級:

```go
type turn struct {
    user      string           // 鐢ㄦ埛杈撳叆锛堢┖琛ㄧず绯荤粺杞锛?    reasoning strings.Builder  // 娴佸紡鎬濊€冭繃绋嬶紙鍙姌鍙狅紝dim 鏍峰紡娓叉煋锛?    answer    strings.Builder  // 娴佸紡鍥炵瓟 + 宸ュ叿/寮曟搸璇婃柇琛?    expanded  bool             // 鐢ㄦ埛鏄惁鐐瑰嚮灞曞紑浜嗘€濊€冨ご閮?    system    bool             // 鏄惁涓虹嫭绔嬪紩鎿庤緭鍑猴紙涓嶅彲鎶樺彔锛屼笉鏄剧ず鐢ㄦ埛杈撳叆锛?}
```

**`Model` 鈥?鏍?Bubble Tea 妯″瀷**锛堟寔鏈夊叏閮?UI 鐘舵€侊級:

```go
type Model struct {
    vp     viewport.Model    // 瀵硅瘽姝ｆ枃婊氳疆瑙嗗彛
    ta     textarea.Model    // 搴曢儴杈撳叆妗?    width  int
    height int
    ready  bool

    // 缁撴瀯鍖栧璇濊浆褰?    turns     []*turn
    streaming bool            // 姝ｅ湪娴佸紡鎺ユ敹涓?    curTurn   int             // 褰撳墠娲昏穬浜ゆ崲杞锛?1 琛ㄧず鏃狅級
    streamTurn int            // 姝ｅ湪鎺ユ敹娴佸紡澧為噺鐨勮疆娆★紙-1 琛ㄧず鏃狅級
    clickMap  map[int]int     // 鍖呰琛?鈫?杞绱㈠紩锛堢敤浜庨紶鏍囩偣鍑绘姌鍙狅級

    status   StatusInfo       // 椤堕儴鐘舵€佹爮鏁版嵁
    task     TaskInfo         // 鍚庡彴浠诲姟闃熷垪蹇収
    history  []HistoryItem    // 鍘嗗彶浼氳瘽鍒楄〃
    commands []CommandItem    // / 鍛戒护闈㈡澘鐩綍
    activity string           // 褰撳墠娲诲姩鎻愮ず琛?
    // Git 闈㈡澘
    gitExpanded bool

    // 妯℃€佽鐩栧眰
    overlay    int            // overlayNone / overlayHistory / overlayCommand / overlayPermission
    search     textinput.Model
    overlayIdx int

    // 鏉冮檺寮圭獥
    permTool  string
    permDesc  string
    permReply chan PermDecision  // 闃诲鐨?worker goroutine 绛夊緟鍥炲鐨勯€氶亾

    // 鍥炶皟
    onSubmit    func(string)     // 鐢ㄦ埛鎻愪氦杈撳叆
    onResume    func(string)     // 鐢ㄦ埛浠庡巻鍙叉仮澶嶄細璇?    onInterrupt func()           // Ctrl+C 涓柇褰撳墠浠诲姟
    quitting    bool
}
```

**`App` 鈥?UI 绋嬪簭鍖呰鍣?*锛坄app.go`锛夛紝鏆撮湶绾跨▼瀹夊叏鐨?Bridge Helpers:

```go
type App struct {
    model   *Model
    program *tea.Program
}

// 鍚庡彴 goroutine 閫氳繃 app.Send* 鎺ㄩ€佹秷鎭埌 UI goroutine
func (a *App) BeginStream(echo string)
func (a *App) Delta(s string)             // 娴佸紡鍥炵瓟澧為噺
func (a *App) Reasoning(s string)         // 娴佸紡鎬濊€冨閲忥紙dim 鏍峰紡锛?func (a *App) EngineLine(s string)        // 寮曟搸璇婃柇琛?func (a *App) EndStream()
func (a *App) SetTask(info TaskInfo)
func (a *App) SetStatus(info StatusInfo)
func (a *App) SetHistory(items []HistoryItem)
func (a *App) SetActivity(s string)
func (a *App) RequestPermission(tool, desc string) PermDecision  // 闃诲寮忔潈闄愬脊绐?```

#### 5.12.4 甯冨眬鍝插

```
鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹? 椤堕儴鐘舵€佹爮 (statusH=1)                     鈹? cove v6.2.1 路 model 路 provider 路 main* 路 鈴?default    杩愯涓?鈿?鈹溾攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹? Git 闈㈡澘锛堝彲閫夛紝鏈夊彉鏇存椂鏄剧ず锛?              鈹? 鈻?宸ヤ綔鍖篬main]鍙樺姩鏂囦欢鍒楄〃 (鍏?涓?
鈹?                                           鈹?   M file1.go
鈹?                                           鈹?   A file2.go
鈹溾攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?                                           鈹?鈹? 瀵硅瘽姝ｆ枃 (viewport, midH = h - 鍏ㄩ儴chrome) 鈹? 鈥?鐢ㄦ埛: 甯垜璇诲彇 main.go
鈹?                                           鈹?鈹?                                           鈹? 鈻?鎬濊€冭繃绋嬶紙鐐瑰嚮灞曞紑锛?鈹?                                           鈹?鈹?                                           鈹? 濂界殑锛宮ain.go 鐨勫唴瀹规槸...
鈹?                                           鈹?鈹溾攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹? 娲诲姩/鎺掗槦琛?(transientH=1锛屽缁堜繚鐣?        鈹? 鈿?鎵ц bash                                 +2 鎺掗槦
鈹溾攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹? 搴曢儴鐘舵€佽 (bottomH=1)                     鈹? 1234 tokens 路 $0.005 路 3.2s    Ctrl+R 鍘嗗彶 路 / 鍛戒护 路 Ctrl+C 閫€鍑?鈹溾攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹? 鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€    鈹?鈹? > 鐢ㄦ埛杈撳叆妗?(inputH=2)                    鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?```

**璁捐鍘熷垯**: 瀵硅瘽姝ｆ枃鍗犳弧鍏ㄥ锛屽彧鏈夎杽钖勭殑 chrome 鐜粫鍛ㄥ洿鈥斺€旈《閮ㄧ姸鎬佹爮銆佷腑閮ㄧ殑 Git 闈㈡澘锛堝彲鍙橈級銆佸彲閫変竴琛屾椿鍔ㄥ尯銆佸簳閮ㄧ姸鎬佽 + 鍒嗗壊绾?+ 杈撳叆妗嗐€?*涓嶄娇鐢ㄤ晶杈规爮鍜屽祵濂楁鏋?*锛屽竷灞€鐢?`layout()` 鏂规硶姣忓抚璁＄畻銆?
甯冨眬涓殑 `transientH=1` **濮嬬粓淇濈暀**锛堝嵆浣挎槸绌鸿锛夛紝闃叉瑙﹀彂鍛戒护鏃跺璇濇鏂囬珮搴︾獊鍙樺鑷磋緭鍏ユ涓婁笅璺冲姩銆?
#### 5.12.5 浜や簰鐗规€?
| 蹇嵎閿?| 鍔熻兘 |
|--------|------|
| **杈撳叆** | `Enter` 鎻愪氦锛堢┖琛屼笉鎻愪氦锛夛紝`Ctrl+J` 鎻掑叆鎹㈣绗?|
| **鎬濊€冩姌鍙?* | 榧犳爣鐐瑰嚮 `鈻?鎬濊€冭繃绋媊 / `鈻?鎬濊€冭繃绋媊 澶撮儴灞曞紑/鎶樺彔 |
| **榧犳爣婊氳疆** | 婊氬姩瀵硅瘽姝ｆ枃瑙嗗彛 |
| **Ctrl+R** | 鎵撳紑鍘嗗彶浼氳瘽鎼滅储瑕嗙洊灞?|
| **`/`**锛堢┖杈撳叆鏃讹級| 鎵撳紑鍛戒护闈㈡澘瑕嗙洊灞傦紙妯＄硦杩囨护锛?|
| **Ctrl+G** | 灞曞紑/鎶樺彔 Git 鐘舵€侀潰鏉?|
| **Ctrl+C** | 浠诲姟杩愯鏃讹細鍙栨秷褰撳墠浠诲姟锛涚┖闂叉椂锛氶€€鍑虹▼搴?|
| **鏉冮檺寮圭獥** | 榧犳爣鐐瑰嚮鎸夐挳鎴栭敭鐩?`y`锛堝厑璁革級銆乣n`锛堟嫆缁濓級銆乣a`锛堝缁堝厑璁革級 |

#### 5.12.6 瑕嗙洊灞傜郴缁燂紙Overlay锛?
涓夌妯℃€佽鐩栧眰锛岀粯鍒跺湪瀵硅瘽姝ｆ枃涔嬩笂锛?
1. **鍘嗗彶鎼滅储**锛坄overlayHistory`锛夛細`Ctrl+R` 鎵撳紑锛屾ā绯婃悳绱細璇濇爣棰橈紝`Enter` 鎭㈠
2. **鍛戒护闈㈡澘**锛坄overlayCommand`锛夛細`/` 鎵撳紑锛屾ā绯婃悳绱㈠懡浠ゅ悕绉板拰鎻忚堪锛宍Enter` 鎵ц
3. **鏉冮檺纭**锛坄overlayPermission`锛夛細宸ュ叿闇€瑕佹巿鏉冩椂寮瑰嚭锛屼笁涓寜閽紙鍏佽/鎷掔粷/濮嬬粓鍏佽锛夛紝work goroutine 琚€氶亾闃诲绛夊緟鐢ㄦ埛鍐崇瓥

瑕嗙洊灞傛縺娲绘椂锛岃緭鍏ユ澶卞幓鐒︾偣锛坄ta.Blur()`锛夛紝鎼滅储妗嗚幏寰楃劍鐐广€傚叧闂鐩栧眰鍚庣劍鐐瑰綊杩樿緭鍏ユ銆?
#### 5.12.7 娴佸紡鏁版嵁娴侊紙Engine 鈫?TUI锛?
```
Engine (worker goroutine)
    鈹?    鈹溾攢 onDelta 鈫?app.Delta(s) 鈫?streamDeltaMsg 鈫?Model.Update()
    鈹?             鈹斺攢 turns[streamTurn].answer 杩藉姞澧為噺
    鈹?             鈹斺攢 refreshViewport(true)  鈫?閲嶆覆鏌?+ 婊氬埌搴曢儴
    鈹?    鈹溾攢 onReasoning 鈫?app.Reasoning(s) 鈫?streamReasoningMsg
    鈹?             鈹斺攢 turns[streamTurn].reasoning 杩藉姞
    鈹?             鈹斺攢 refreshViewport(true)
    鈹?    鈹溾攢 onEngineOutput 鈫?app.EngineLine(s) 鈫?engineLineMsg
    鈹?             鈹斺攢 杩藉姞鍒?streamTurn锛堝湪娴佷腑锛? curTurn锛堟湁褰撳墠杞锛? appendSystem锛堢郴缁熻疆娆★級
    鈹?    鈹溾攢 寮€濮嬫祦:     app.BeginStream("") 鈫?streamBeginMsg 鈫?streaming=true, 鍒涘缓鏂?turn
    鈹斺攢 缁撴潫娴?     app.EndStream()     鈫?streamEndMsg   鈫?streaming=false
```

**鍏抽敭璁捐**: 鎬濊€冭繃绋嬶紙reasoning锛夊湪**鍥炵瓟鍐呭鍒拌揪鍓?*瀹炴椂娓叉煋涓哄睍寮€鐘舵€侊紱涓€鏃﹀洖绛斿唴瀹瑰嚭鐜帮紙鎴栨祦缁撴潫锛夛紝鎬濊€冭繃绋嬫姌鍙犱负涓€琛?`鈻?鎬濊€冭繃绋嬶紙鐐瑰嚮灞曞紑锛塦锛岀敤鎴峰彲鐐瑰嚮鍐嶆鎵撳紑銆?
#### 5.12.8 浠诲姟闃熷垪锛坄tuiJobQueue`锛?
FIFO 闃熷垪 + 鏉′欢鍙橀噺闃诲锛屼繚璇佺敤鎴锋彁浜ゅ拰寮曟搸璋冪敤涓茶鍖栵細

```go
type tuiJobQueue struct {
    mu     sync.Mutex
    cond   *sync.Cond
    items  []string
    closed bool
}
```

- `push(s)`: 杩藉姞鍒伴槦灏撅紝`cond.Signal()` 鍞ら啋 worker
- `pushFront(s)`: 鎻掑叆闃熼锛堢敤鎴蜂腑鏂悗閲嶆柊鎻愪氦锛?- `pop()`: 闃诲绛夊緟锛岃繑鍥炲綋鍓嶉」 + 鍓╀綑闃熷垪蹇収锛堢敤浜庝晶杈规爮锛?
鍗?worker goroutine 浠庨槦鍒楀脊鍑哄苟涓茶澶勭悊姣忎釜鎻愪氦銆?
#### 5.12.9 `runTUI()` 鍚姩娴佺▼锛坄repl_tui.go:101`锛?
```
1. 鍒涘缓 tuiJobQueue
2. 鍒涘缓 tui.App锛氱粦瀹?onSubmit锛堝叆闃熺敤鎴疯緭鍏ワ級銆乷nResume锛堟仮澶嶅巻鍙蹭細璇濓級銆乷nInterrupt锛堝彇娑堟鍦ㄨ繍琛岀殑浠诲姟锛?3. 璁剧疆 eng.PermissionPrompt 鈫?app.RequestPermission()锛堥樆濉炲紡鏉冮檺寮圭獥锛?4. 鍚姩鍚庡彴 goroutine锛?   - Git 鐘舵€佸埛鏂帮紙姣?2 绉掞級
   - 鍘嗗彶浼氳瘽鍒楄〃鍔犺浇锛圕trl+R 瑕嗙洊灞傛暟鎹簮锛?5. 鍚姩 worker goroutine锛?   - 寰幆 pop 闃熷垪
   - / 鍛戒护锛熲啋 鍚屾鎵ц锛堜笌寮曟搸璋冪敤涓茶锛岄伩鍏嶇姸鎬佺珵浜夛級
   - 鏅€氳緭鍏ワ紵鈫?棰勭畻/API Key 棰勬 鈫?eng.RunMessageWithStream() 鈫?娴佸紡妗ユ帴
   - 鑷姩淇濆瓨浼氳瘽
6. 鍚姩绉嶅瓙 goroutine锛氬皢 banner + 璇婃柇淇℃伅 + 鑽夌鎻愮ず鍐欏叆瀵硅瘽姝ｆ枃
7. app.Run() 杩涘叆 Bubble Tea 浜嬩欢寰幆锛堥樆濉炵洿鍒伴€€鍑猴級
```

#### 5.12.10 鏍峰紡绯荤粺锛坄styles.go`锛?
```go
statusBarStyle  // 椤堕儴鐘舵€佹爮锛氭殫鑹叉枃瀛?+ 闈掕壊鑳屾櫙锛堜笌 Cove Logo 鍚岃壊锛?userStyle       // 鐢ㄦ埛杈撳叆锛氶潚鑹茬矖浣?dimStyle        // 娆¤鏂囨湰/鎬濊€冭繃绋嬶細鐏拌壊
thinkHeaderStyle // 鍙偣鍑绘姌鍙犲ご閮細鐏拌壊鏂滀綋
activityStyle   // 娲诲姩鎸囩ず琛岋細闈掕壊
overlayBoxStyle // 瑕嗙洊灞傦細鍦嗚杈规 + 闈掕壊杈规鑹?selectedStyle   // 瑕嗙洊灞傞€変腑椤癸細鐧借壊鏂囧瓧 + 闈掕壊鑳屾櫙
btnAllowStyle   // 鏉冮檺銆屽厑璁搞€嶆寜閽細鐧借壊 + 缁胯壊鑳屾櫙
btnDenyStyle    // 鏉冮檺銆屾嫆缁濄€嶆寜閽細鐧借壊 + 绾㈣壊鑳屾櫙
btnAlwaysStyle  // 鏉冮檺銆屽缁堝厑璁搞€嶆寜閽細鐧借壊 + 鐞ョ弨鑳屾櫙
```

鍏夋爣浣跨敤**鐪熷疄缁堢鍏夋爣**锛坄ta.SetVirtualCursor(false)`锛夛紝杩欎娇 CJK IME 鑳藉湪姝ｇ‘浣嶇疆缁樺埗棰勭紪杈戞枃鏈紙鎷奸煶绛夛級銆?
---

### 5.13 REPL 浜や簰灞傦紙鈽?闄嶇骇涓?Fallback锛?
#### 5.13.1 棰滆壊宸ュ叿锛坈olor.go锛?
**鏂囦欢**: `internal/repl/color.go`锛?69 琛岋級

> **娉ㄦ剰**: REPL 鐜板湪鏄?fallback 妯″紡锛堜粎绠￠亾/閲嶅畾鍚?`--no-tui` 鏃朵娇鐢級銆備絾 `repl.Banner()`銆乣repl.PrintSafe()`銆乣repl.PrintAbove()` 绛夊伐鍏峰嚱鏁板湪 TUI 妯″紡涓嬩粛琚?`main.go` 璋冪敤锛岀敤浜庣敓鎴?banner 鏂囨湰鍜?plan 妯″紡杈撳嚭銆?
ANSI 棰滆壊鍜屾牱寮忓畾涔夛細

```go
// 棰滆壊甯搁噺
const (
    Reset   = "\033[0m"
    Red     = "\033[31m"
    Green   = "\033[32m"
    Yellow  = "\033[33m"
    Blue    = "\033[34m"
    Cyan    = "\033[36m"
    Gray    = "\033[90m"
    // ...
)

// 娓叉煋鍑芥暟
func Dim(s string) string      // 鐏拌壊/鏆楄壊鏂囨湰
func Bold(s string) string     // 绮椾綋
func Highlight(s string) string // 楂樹寒锛堥潚鑹茬矖浣擄級
func Error(s string) string    // 閿欒锛堢孩鑹诧級
```

#### 5.13.2 琛岀紪杈戝櫒锛坮eadline.go锛?
**鏂囦欢**: `internal/repl/readline.go`锛?29 琛岋級

鑷畾涔夌粓绔缂栬緫鍣紝鏀寔锛?
- **鍏夋爣绉诲姩**: 宸﹀彸绠ご銆丠ome/End銆丆trl+A/E
- **缂栬緫鎿嶄綔**: 閫€鏍笺€佸垹闄ゃ€丆trl+W锛堝垹闄よ瘝锛夈€丆trl+U锛堝垹闄ゅ埌琛岄锛?- **鍘嗗彶瀵艰埅**: 涓婁笅绠ご娴忚鍘嗗彶鍛戒护
- **澶氳杈撳叆**: 浠?`\` 缁撳熬鑷姩缁
- **鑷姩琛ュ叏**: Tab 琛ュ叏鏂囦欢璺緞鍜屽懡浠?- **璇硶楂樹寒**: 瀵圭壒娈婂懡浠よ繘琛岀潃鑹?
---

### 5.14 Context 涓婁笅鏂囩鐞?
**鏂囦欢**: `internal/context/context.go`锛?21 琛岋級

璐熻矗鍒嗘瀽椤圭洰鐩綍缁撴瀯骞剁敓鎴愮粰 AI 鐪嬬殑涓婁笅鏂囷細

```go
type ProjectContext struct {
    Root       string           // 椤圭洰鏍圭洰褰?    Language   string           // 妫€娴嬪埌鐨勭紪绋嬭瑷€
    FileTree   []FileEntry      // 鏂囦欢鏍?    Framework  string           // 妫€娴嬪埌鐨勬鏋?}

func Analyze(dir string) (*ProjectContext, error)
func (pc *ProjectContext) Format() string  // 鏍煎紡鍖栦负 AI 鍙瀛楃涓?```

**妫€娴嬮€昏緫**:
- 鎵弿鏍圭洰褰曞叧閿枃浠讹紙`go.mod` 鈫?Go, `package.json` 鈫?Node.js, etc.锛?- 蹇界暐 `.gitignore` 鍜?`.coveignore` 涓寚瀹氱殑鏂囦欢
- 鐢熸垚绠€娲佺殑鏂囦欢鏍?
**RepoMap**锛坄internal/repomap/repomap.go`, 397 琛岋級:
- 鐢熸垚浠ｇ爜搴撶殑缁撴瀯鍦板浘
- 鍖呭惈鍏抽敭绫?鍑芥暟/妯″潡鐨勫畾浣嶄俊鎭?- 甯姪 AI 鐞嗚В椤圭洰缁撴瀯

---

### 5.15 杈呭姪妯″潡

#### 5.15.1 鏃ュ織绯荤粺锛坙og/logger.go锛?
- 鍥涚骇鏃ュ織锛欴ebug, Info, Warn, Error
- 杈撳嚭鍒?stderr锛堜笌 stdout 鐨?AI 杈撳嚭鍒嗙锛?- `SetSink()` 鏈哄埗锛歐arn/Error 鑷姩鍥炶皟锛堢敤浜庤瘖鏂褰曪級

#### 5.15.2 閰嶇疆绠＄悊锛坈onfig/config.go锛?
```go
type Config struct {
    Model          string          // AI 妯″瀷鍚嶇О
    Provider       ProviderConfig  // Provider 閰嶇疆
    Tools          []tool.Tool     // 宸ュ叿鍒楄〃
    PermissionMode string          // "default" / "auto" / "strict"
    MaxBudget      float64         // 鏈€澶ц垂鐢ㄩ绠楋紙缇庡厓锛?    MaxSteps       int             // 鏈€澶у伐鍏疯皟鐢ㄦ鏁?    TUI            bool            // 鏄惁鍚敤 TUI 妯″紡
    NoTUI          bool            // 鏄惁绂佺敤 TUI 妯″紡
    // ...
}
```

#### 5.15.3 鎴愭湰杩借釜锛坈ost/tracker.go锛?
杩借釜 API 璋冪敤璐圭敤锛?
```go
type Tracker struct {
    totalCost float64
    modelRates map[string]Rate  // 鍚勬ā鍨嬩环鏍?}

type Rate struct {
    InputPrice  float64  // 姣?1K tokens 浠锋牸
    OutputPrice float64
}
```

#### 5.15.4 妫€鏌ョ偣锛坈heckpoint/checkpoint.go锛?
鏂囦欢淇敼鍓嶈嚜鍔ㄥ浠斤細

```go
func Save(filePath string) error {
    // 灏嗘枃浠跺鍒跺埌 ~/.cove/checkpoints/{timestamp}/{path}
}
```

#### 5.15.5 璇婃柇绯荤粺锛坉iagnostic/锛?
- `checker.go`: 绯荤粺鍋ュ悍妫€鏌ワ紙API 杩為€氭€с€佸伐鍏峰彲鐢ㄦ€э級
- `recorder.go`: 杩愯鏃朵簨浠惰褰曪紙鐢ㄤ簬浜嬪悗璋冭瘯锛?- `errors.go`: 閿欒鐮佸畾涔?
#### 5.15.6 瀹夊叏妫€鏌ワ紙guardrail/锛?
```go
func CheckInput(input string) error     // 妫€鏌ョ敤鎴疯緭鍏?func CheckOutput(output string) error   // 妫€鏌?AI 杈撳嚭
```

妫€娴嬫綔鍦ㄧ殑瀹夊叏闂锛堟敞鍏ャ€佹晱鎰熶俊鎭硠闇茬瓑锛夈€?
#### 5.15.7 鎻掍欢绯荤粺锛坧lugin/plugin.go, 514 琛岋級

鏀寔澶栭儴鎻掍欢鎵╁睍锛?
```go
type Plugin struct {
    Name    string
    Tools   []tool.Tool
    Hooks   []Hook
    Skills  []skills.Skill
}

func Load(dir string) ([]Plugin, error)
```

---

## 6. 鏍稿績鏁版嵁娴?
### 6.1 涓€娆″畬鏁村璇濈殑鏁版嵁娴?
```
鐢ㄦ埛杈撳叆 "甯垜璇讳竴涓?main.go"
    鈹?    鈻?鈹屸攢 REPL Loop 鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?1. 璇诲彇鐢ㄦ埛杈撳叆                                        鈹?鈹?2. 妫€鏌ユ槸鍚︽槸鍐呯疆鍛戒护锛?help, /exit...锛?               鈹?鈹?3. 鏋勫缓 api.Message{Role: "user", Content: "甯垜..."}  鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?                    鈹?                    鈻?鈹屸攢 Engine.RunMessageWithStream 鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?鈹?                                                      鈹?鈹?鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?     鈹?鈹?鈹?鏋勫缓 System Prompt:                           鈹?     鈹?鈹?鈹?- 瑙掕壊瀹氫箟                                    鈹?     鈹?鈹?鈹?- 宸ュ叿鍒楄〃 (Defs)                             鈹?     鈹?鈹?鈹?- 椤圭洰涓婁笅鏂?(Context.Format())               鈹?     鈹?鈹?鈹?- 浠撳簱鍦板浘 (RepoMap)                          鈹?     鈹?鈹?鈹?- 鐩稿叧璁板繂 (Memory.Search())                  鈹?     鈹?鈹?鈹?- 鍙敤鎶€鑳?(Skills.ListForAI())              鈹?     鈹?鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?     鈹?鈹?                      鈹?                              鈹?鈹?                      鈻?                              鈹?鈹?鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?     鈹?鈹?鈹?API Call: Provider.ChatStream()              鈹?     鈹?鈹?鈹?鈫?POST https://api.anthropic.com/v1/messages 鈹?     鈹?鈹?鈹?鈫?SSE Stream 杩斿洖                            鈹?     鈹?鈹?鈹?鈫?瑙ｆ瀽: content_block_delta / tool_use       鈹?     鈹?鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?     鈹?鈹?                      鈹?                              鈹?鈹?           鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹粹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?                   鈹?鈹?           鈹?                    鈹?                    鈹?鈹?     杩斿洖鏂囨湰             杩斿洖宸ュ叿璋冪敤                  鈹?鈹?           鈹?                    鈹?                    鈹?鈹?           鈻?                    鈻?                    鈹?鈹?  onDelta(鏂囨湰)        鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?          鈹?鈹?  瀹炴椂杈撳嚭缁欑敤鎴?        鈹?鏉冮檺妫€鏌?          鈹?          鈹?鈹?           鈹?           鈹?鈫?                鈹?          鈹?鈹?           鈹?           鈹?PermissionPrompt  鈹?          鈹?鈹?           鈹?           鈹?鈫?                鈹?          鈹?鈹?           鈹?           鈹?骞惰鎵ц宸ュ叿       鈹?          鈹?鈹?           鈹?           鈹?鈫?                鈹?          鈹?鈹?           鈹?           鈹?鏀堕泦缁撴灉           鈹?          鈹?鈹?           鈹?           鈹?鈫?                鈹?          鈹?鈹?           鈹?           鈹?缁撴灉娣诲姞鍒版秷鎭巻鍙? 鈹?          鈹?鈹?           鈹?           鈹?鈫?                鈹?          鈹?鈹?           鈹?           鈹?鍐嶆璋冪敤 AI       鈹傗攢鈹€鈹?       鈹?鈹?           鈹?           鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹? 鈹?       鈹?鈹?           鈹?                    鈹?            鈹?       鈹?鈹?           鈹?                    鈼勨攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?       鈹?鈹?           鈹?             (寰幆鐩村埌 AI 鍋滄璋冪敤宸ュ叿)     鈹?鈹?           鈻?                                          鈹?鈹?  鏈€缁堝搷搴旀枃鏈?                                         鈹?鈹?                                                      鈹?鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?                    鈹?                    鈻?    鈹屸攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?    鈹?鍚庡鐞?                       鈹?    鈹?- 鏇存柊鎴愭湰 Tracker            鈹?    鈹?- 瑙﹀彂鍚庡彴鍥為【 Review         鈹?    鈹?- 淇濆瓨 Session                鈹?    鈹?- 鏇存柊 Memory/Skills          鈹?    鈹斺攢鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹€鈹?                    鈹?                    鈻?              杩斿洖鍝嶅簲缁?REPL
```

### 6.2 宸ュ叿鎵ц鐨勬暟鎹祦

```
Engine 鏀跺埌 AI 鐨勫伐鍏疯皟鐢ㄨ姹?    鈹?    鈹溾攢 鎻愬彇 tool_calls[] 鈫?[{name: "read", input: {filePath: "..."}}, ...]
    鈹?    鈹溾攢 瀵规瘡涓?tool_call 骞跺彂鎵ц:
    鈹?  鈹?    鈹?  鈹溾攢 toolReg.Get(name)  鈫?鑾峰彇 Tool 瀹炰緥
    鈹?  鈹溾攢 tool.Validate(input) 鈫?鍙傛暟楠岃瘉
    鈹?  鈹溾攢 tool.CheckPermissions(input, ctx) 鈫?鏉冮檺妫€鏌?    鈹?  鈹?  鈹溾攢 Allow 鈫?鐩存帴鎵ц
    鈹?  鈹?  鈹溾攢 Deny  鈫?杩斿洖鎷掔粷鍘熷洜
    鈹?  鈹?  鈹斺攢 Ask   鈫?璋冪敤 PermissionPrompt锛堝彲鑳介樆濉炵瓑寰呯敤鎴疯緭鍏ワ級
    鈹?  鈹溾攢 tool.Call(ctx, input, ctx) 鈫?鎵ц宸ュ叿
    鈹?  鈹斺攢 杩斿洖 Result{Data: "..."} 鎴?error
    鈹?    鈹溾攢 鏀堕泦鎵€鏈夌粨鏋?    鈹?    鈹溾攢 鏋勯€?tool result 娑堟伅:
    鈹?  api.Message{Role: "tool", ToolCallID: tc.ID, Content: result.Data}
    鈹?    鈹溾攢 杩藉姞鍒?e.messages
    鈹?    鈹斺攢 鍐嶆璋冪敤 Provider.ChatStream()锛堝皢宸ュ叿缁撴灉鎻愪氦缁?AI锛?```

---

## 7. 鍏抽敭璁捐妯″紡

### 7.1 鎺ュ彛鎶借薄妯″紡

鎵€鏈夊彲鏇挎崲缁勪欢閮藉畾涔夋帴鍙ｏ細

```go
// AI Provider 鍙浛鎹?type Provider interface { Chat(...); ChatStream(...) }

// 宸ュ叿鍙墿灞?type Tool interface { Def(); Validate(); CheckPermissions(); Call() }

// 宓屽叆鎻愪緵鑰呭彲鏇挎崲
type EmbeddingProvider interface { Embed(); Dim() }
```

### 7.2 娉ㄥ唽琛ㄦā寮?
宸ュ叿閫氳繃娉ㄥ唽琛ㄧ鐞嗭細

```go
reg := tool.NewRegistry()
reg.Register(&BashTool{})
reg.Register(&ReadTool{})
// ... 
engine.SetRegistry(reg)
```

### 7.3 鍥炶皟/閽╁瓙妯″紡

Engine 鏆撮湶閽╁瓙渚涘閮ㄥ畾鍒讹細

```go
eng.PermissionPrompt = myPermissionHandler
eng.OnDelta = myStreamHandler
```

### 7.4 浼橀泤闄嶇骇妯″紡

```go
if e.memStore != nil {  // 璁板繂绯荤粺鍙€?    memories := e.memStore.Search(query, 5)
    // 娉ㄥ叆鍒扮郴缁熸彁绀?}
```

### 7.5 Panic 鎭㈠妯″紡

鎵€鏈夊伐鍏锋墽琛岄兘鍦?goroutine 涓湁 panic 鎭㈠锛?
```go
go func() {
    defer func() {
        if r := recover(); r != nil {
            log.Errorf("tool panic: %v", r)
            results[idx] = tool.Result{IsError: true, Data: fmt.Sprint(r)}
        }
    }()
    results[idx] = tool.Call(...)
}()
```

### 7.6 涓婁笅鏂囦紶鎾ā寮?
鎵€鏈夊紓姝ユ搷浣滈€氳繃 `context.Context` 浼犳挱鍙栨秷淇″彿锛?
```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
// 浼犻€掔粰鎵€鏈夊瓙鎿嶄綔
```

---

## 8. 濡備綍杩愯涓庢瀯寤?
### 8.1 鐜瑕佹眰

- Go 1.22+
- Git锛堢敤浜?worktree 鍔熻兘锛?- ripgrep锛坄rg` 鍛戒护锛岀敤浜?grep 宸ュ叿锛?
### 8.2 閰嶇疆

鍒涘缓 `~/.cove/config.json`:

```json
{
    "provider": {
        "name": "anthropic",
        "api_key": "sk-ant-xxx",
        "model": "claude-sonnet-4-20250514"
    },
    "permission_mode": "default",
    "max_budget": 10.0
}
```

鎴栦娇鐢ㄧ幆澧冨彉閲忥細
```bash
export ANTHROPIC_API_KEY="sk-ant-xxx"
export COVE_MODEL="claude-sonnet-4-20250514"
```

### 8.3 鏋勫缓

```bash
# 鍩虹鏋勫缓
cd G:\github\cove\agent
go build -o cove.exe ./cli/cove/

# 鍖呭惈 Headless Chrome 鏀寔锛堝彲閫夛級
go build -tags chromedp -o cove.exe ./cli/cove/

# 杩愯
./cove.exe
```

### 8.4 寮€鍙戞ā寮?
```bash
# 鐩存帴杩愯锛堟棤闇€鏋勫缓锛?go run ./cli/cove/

# 杩愯娴嬭瘯
go test ./internal/...

# 杩愯鐗瑰畾鍖呮祴璇?go test ./internal/engine/ -v -run TestEngineBasicMessageFlow
```

---

## 9. 濡備綍鎵╁睍绯荤粺

### 9.1 娣诲姞鏂板伐鍏?
```go
// 1. 鍒涘缓鏂版枃浠?internal/tool/my_tool.go
package tool

type MyTool struct{}

func (t *MyTool) Def() Def {
    return Def{
        Name:        "my_tool",
        Description: "鎴戠殑鑷畾涔夊伐鍏?,
        InputSchema: json.RawMessage(`{
            "type": "object",
            "properties": {
                "input": {"type": "string", "description": "杈撳叆鍙傛暟"}
            },
            "required": ["input"]
        }`),
        IsReadOnly:  true,
    }
}

func (t *MyTool) Validate(input Input) string { return "" }

func (t *MyTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
    return PermissionDecision{Decision: Allow}
}

func (t *MyTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
    // 瀹炵幇閫昏緫
    return Result{Data: "缁撴灉"}, nil
}

// 2. 鍦?registry.go 涓敞鍐?// reg.Register(&MyTool{})
```

### 9.2 娣诲姞鏂?AI Provider

```go
// 1. 鍒涘缓 internal/api/my_provider.go
type MyProvider struct { ... }

func (p *MyProvider) Name() string { return "my_provider" }
func (p *MyProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) { ... }
func (p *MyProvider) ChatStream(ctx context.Context, req ChatRequest, h StreamHandler) (*ChatResponse, error) { ... }

// 2. 鍦?provider_catalog.go 涓坊鍔?// catalog["my_provider"] = func(cfg) Provider { return NewMyProvider(cfg) }
```

### 9.3 娣诲姞 MCP 鏈嶅姟鍣?
鍦ㄩ厤缃枃浠朵腑娣诲姞锛?
```json
{
    "mcp_servers": [
        {
            "command": "npx",
            "args": ["-y", "@anthropic/mcp-server-filesystem", "/path/to/dir"]
        }
    ]
}
```

---

## 10. 娴嬭瘯绛栫暐

### 10.1 鍗曞厓娴嬭瘯

- **Engine 娴嬭瘯** (`engine_test.go`, 1076 琛?: 浣跨敤 Mock Provider 鍜?Mock Tool 杩涜闆嗘垚娴嬭瘯
- **Skill 娴嬭瘯** (`skills_test.go`): 娴嬭瘯鎶€鑳藉姞杞藉拰娉ㄥ唽
- **Session 娴嬭瘯** (`store_test.go`): 娴嬭瘯浼氳瘽鎸佷箙鍖?- **Diagnostic 娴嬭瘯** (`diagnostic_test.go`): 娴嬭瘯璇婃柇鍔熻兘

### 10.2 Mock 绛栫暐

```go
// Mock Provider - 妯℃嫙 AI 鍝嶅簲
type mockProvider struct {
    responses []mockResponse  // 棰勮鐨勫搷搴旈槦鍒?}

// Mock Tool - 鍙帶鐨勫伐鍏疯涓?type mockTool struct {
    name     string
    readOnly bool
    result   string
    err      error
    panicMsg string  // 娴嬭瘯 panic 鎭㈠
    delay    time.Duration  // 妯℃嫙鎱㈤€熷伐鍏?}
```

### 10.3 鍏抽敭娴嬭瘯鍦烘櫙

| 娴嬭瘯鐢ㄤ緥 | 娴嬭瘯鍐呭 |
|----------|----------|
| `TestEngineBasicMessageFlow` | 鍩烘湰娑堟伅娴佺▼ |
| `TestEngineToolExecution` | 宸ュ叿璋冪敤鎵ц |
| `TestEnginePermissionDenied` | 鏉冮檺鎷掔粷 |
| `TestEngineToolPanicRecovery` | 宸ュ叿 Panic 鎭㈠ |
| `TestEngineMultipleIterations` | 澶氳疆宸ュ叿璋冪敤 |
| `TestEngineAPIError` | API 閿欒澶勭悊 |
| `TestEngineContextCancellation` | 涓婁笅鏂囧彇娑?|
| `TestEnginePermissionPromptNil` | 鏉冮檺閽╁瓙鏈缃?|

### 10.4 杩愯娴嬭瘯

```bash
# 鍏ㄩ儴娴嬭瘯
go test ./...

# 鎸囧畾娴嬭瘯
go test ./internal/engine/ -v -run "TestEngine"

# 甯﹁鐩栫巼
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out
```

---

## 闄勫綍 A: 鍏抽敭甯搁噺

| 甯搁噺 | 鍊?| 璇存槑 |
|------|-----|------|
| `maxContextMessages` | 200 | 鏈€澶т笂涓嬫枃娑堟伅鏁?|
| `maxContextTokens` | 90000 | 鏈€澶т笂涓嬫枃 Token 鏁?|
| `stallThreshold` | 30s | 鍗￠】妫€娴嬮槇鍊?|
| `maxBodySize` | 5MB | 缃戦〉鎶撳彇鏈€澶т綋绉?|
| `outputLimit` | 100KB | 缃戦〉杈撳嚭鎴柇 |
| `defaultDim` | 384 | 鍚戦噺宓屽叆缁村害 |
| `reviewInterval` | 4 messages | 鍚庡彴鍥為【瑙﹀彂闂撮殧 |
| `reviewTimeout` | 30s | 鍚庡彴鍥為【瓒呮椂 |

## 闄勫綍 B: 鐜鍙橀噺

| 鍙橀噺 | 璇存槑 |
|------|------|
| `ANTHROPIC_API_KEY` | Anthropic API Key |
| `OPENAI_API_KEY` | OpenAI API Key |
| `OPENAI_BASE_URL` | OpenAI 鍏煎绔偣 |
| `OPENAI_MODEL` | OpenAI 妯″瀷鍚嶇О |
| `COVE_MODEL` | 瑕嗙洊妯″瀷閫夋嫨 |
| `COVE_PROVIDER` | 瑕嗙洊 Provider 閫夋嫨 |

---

> **鏂囨。鐗堟湰**: 1.0  
> **鏈€鍚庢洿鏂?*: 2025  
> **閫傜敤浠ｇ爜鐗堟湰**: cove/agent (G:\github\cove\agent)

