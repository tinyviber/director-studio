# Director Studio — MyGo 导演工作台

连接 agent 制作与人工审核的桌面工作台：台本编辑、Draft Animatic 连续预演、
Clips 时间线编排、VoiceMorph 配音（录音/变声/ASR 对齐）、带上下文的导演批注。

## 为什么

agent 完整生成一条视频要数小时；人类看到成片才发现问题再返工，成本太高。
本工作台把审核点提前：agent 只产出台本 + draft 结构，工作台实时画出火柴人
预演（不调任何生成模型），人对着台本配音、听节奏、标问题，再把反馈导回给
agent 做局部修改。Draft 批准后才进入正式生成，且素材替换按 clip 粒度进行。

## 启动与构建

```bash
cd director-studio
bun install                    # 一次性
bun run dev                    # vite dev + mygo dev（webview 实时加载）
bun run build                  # 产出 build/darwin-*/DirectorStudio.app + dmg
```

调试/查看 UI 也可以纯浏览器：`bun run dev:web`（vite :5174）——此时走 mock
桥，编辑在内存生效，录音/ASR/文件访问不可用（界面会标注）。

```
go build . && ./studio genworkbench /path/to/workspace   # 生成示例场景
```

打开目录结构约定：

```
<workspace>/
  scenes/<scene_id>/scene.json   # 台本+clips+编排+音频状态+反馈（单一数据源）
  audio/rec/                     # 录音 wav
  audio/dubbing/<session>/       # dubbing.Analyze 输出（asr-raw.json/timeline.json）
  exports/                       # 导出的反馈任务 fb-*.json/.md
```

仓库内自带 `examples/animatic-demo/workbench/`（由 `genworkbench` 生成）。

## 架构

- `main.go` — mygo.Bind(&Studio{}) + 窗口；`genworkbench` 子命令。
- `scene.go` — Scene/Clip/BeatEvent/Feedback 模型 + 校验 + 编辑操作
  （reorder/split/delete/duplicate/trim/set_source/update_line/add_feedback…）。
  所有编辑经 `SceneEdit(opJSON)` 入口，写盘前快照进 undo 栈（⌘Z/⇧⌘Z）。
- `draft.go` — 台本 → draft spec：台词按语速估时、聚合 ≤9s clips、
  方向词→参数化动作（walk/sit/point/talk/turn/surprise/enter/exit…）、
  默认 wide 镜头 + 换说话人轮换中景/特写。重生成时按 line_ids 集合迁移
  已绑定素材与状态——final 绑定不因重排丢失。
- `service.go` — 绑定方法全集（JSON 字符串 in/out，同 voice-morph 约定）。
  配音链路直接调 `voice-morph/dubbing`（Analyze/Reconcile/Render/模型管理）
  与 `voice-morph/engine`（DSP 变声预览）；不重复实现 DSP/ASR。
- `media.go` — 127.0.0.1 随机端口媒体服务：白名单根目录 + Range（拖拽进度
  即真 seek）。webview 音频/视频/图片都经它出文件。
- `web/` — Vite + 原生 TS：canvas 火柴人播放器（真实时钟 rAF、台词音频按
  beat 调度 rendered/take 音频）、拖拽时间线（HTML5 DnD）、台本面板（点击
  定位播放头/双击编辑）、配音面板（录音 take/ASR 对齐审核表/渲染）、批注
  面板（自动带 clip_id+line_id+时间戳上下文，导出 agent 任务文件）。

## 与 voice-morph 的依赖

`go.mod`：`replace voice-morph => ../voice-morph`（同机相对路径，
clone 布局 `repos/{director-studio,voice-morph}`）。复用 `dubbing` 与 `engine`
两个库包；不 import 其 `package main` 的绑定层（Morpher/Dubber）。
ASR worker `asr/dub_worker.py` 是从 voice-morph vendored 的副本（打包进
`Resources/asr/`；改动请同步上游）。模型走 voice-morph 的 `dubbing` 用户配置
（`~/Library/Application Support/voice-morph/dubbing.json`），模型缺失时
`配音` 面板会显示状态，可用「设置模型目录」或环境变量
`VOICEMORPH_SENSEVOICE_DIR` / `VOICEMORPH_VAD_DIR` 指到本地缓存。

## 录音与 ASR 注意

- 麦克风权限走 mygo.config.ts 的 NSMicrophoneUsageDescription；首次录音
  弹系统授权。
- ASR 需要装了 funasr 的 Python 以及 SenseVoiceSmall/FSMN-VAD 模型
  （modelscope 缓存或 `models set`）。没有这些时录音/导入 wav/编辑仍可用，
  「ASR 对齐台本」会报 E_MODEL_NOT_FOUND——这是环境限制，不是应用 bug。
- Draft 里台词时长是估算值；跑过 ASR 对齐后，时间线中各句的**真实录音
  时长**可在渲染产物 manifest 里查到（lines/<seg>-*.wav 的 cut_start/end）。

## 测试

```bash
CGO_ENABLED=0 go test .          # 纯 Go：模型/编辑/持久化/dubbing 渲染链路
bun run typecheck                # tsc --noEmit
bun run build:web                # vite 产物
```

`studio_test.go` 走 voice-morph 的 dub-example fixture：真实 Reconcile +
Render（DSP），不跑真 ASR（funasr 依赖 = 环境限制）。
