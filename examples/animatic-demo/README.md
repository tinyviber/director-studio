# animatic-demo — Director Studio 示例工作区

`workbench/` 由 `kit/studio` 的 `genworkbench` 子命令生成，供导演工作台开箱使用：

```bash
cd kit/studio && go build . && ./studio genworkbench \
  /Users/wj/Documents/repos/director-studio/examples/animatic-demo/workbench
```

- `scenes/demo-door/` — 10 行台本 / 2 角色（摄影师 + 悟空）/ 5 个 draft clips，
  含一条逻辑类导演批注（"开门前缺察觉异常镜头"）。
- `scenes/demo-voice/` — 配音链路示例：fixture take.wav + asr-raw.json +
  真实 Reconcile 产出的 timeline.json + 真实 DSP Render 产物
  （lines/*.wav、roles/*.wav、mix.wav、subtitles.srt、manifest.json）。
  打开即可在配音面板试听对齐结果——ASR 数字是离线样例，不是真实识别。
- `audio/rec/take-demo.wav` — fixture 录音（合成语音，非真人）。
