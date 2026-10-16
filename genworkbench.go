// genworkbench.go — `studio genworkbench <dir>` 生成示例 workspace：
// 一个 2 角色、多台词、含动作/镜头提示的 scene + draft clips。
// 走与 UI 相同的 GenDraft 代码路径，保证产物一致。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"voice-morph/dubbing"
)

func genWorkbench(dir string) error {
	sc := &Scene{
		Version: SceneVersion, SceneID: "demo-door", Title: "示例 · 门口的异动",
		Roles: []Role{
			{ID: "photo", Name: "摄影师", Color: "#4a90d9",
				Voice: &dubbing.VoiceProfile{Backend: dubbing.VoiceBackendDSP, Preset: "normal"}},
			{ID: "wukong", Name: "悟空", Color: "#d9634a",
				Voice: &dubbing.VoiceProfile{Backend: dubbing.VoiceBackendDSP, Preset: "woman"}},
		},
		Script: dubbing.Script{Version: 1, Lines: []dubbing.ScriptLine{
			{LineID: "l01", SceneID: "demo-door", RoleID: "photo", Text: "巷子里怎么这么安静。", Direction: "环顾"},
			{LineID: "l02", SceneID: "demo-door", RoleID: "wukong", Text: "师父说今晚要拍的就是这儿。", Direction: "蹲在墙头"},
			{LineID: "l03", SceneID: "demo-door", RoleID: "photo", Text: "你先下来，我去拿灯。"},
			{LineID: "l04", SceneID: "demo-door", RoleID: "wukong", Text: "灯不够亮的话，我可以变个大的。", Direction: "跳下"},
			{LineID: "l05", SceneID: "demo-door", RoleID: "photo", Text: "等等，那是什么声音？", Direction: "回头"},
			{LineID: "l06", SceneID: "demo-door", RoleID: "wukong", Text: "别动，有人在里面。", Direction: "警觉"},
			{LineID: "l07", SceneID: "demo-door", RoleID: "photo", Text: "要不要先报警？", Direction: "压低声音"},
			{LineID: "l08", SceneID: "demo-door", RoleID: "wukong", Text: "不用，看我的。"},
			{LineID: "l09", SceneID: "demo-door", RoleID: "photo", Text: "门开了——里面站着一个戴帽子的人。", Direction: "全景"},
			{LineID: "l10", SceneID: "demo-door", RoleID: "wukong", Text: "那是师父安排的内应。", Direction: "笑"},
		}},
	}
	clips, warns, err := GenDraft(sc, nil)
	if err != nil {
		return err
	}
	sc.Clips = clips
	for _, c := range clips {
		sc.Order = append(sc.Order, c.ClipID)
	}
	// 样例反馈：剧情跳跃（摄影师还没看到变身就生气）
	sc.Feedback = []Feedback{{
		ID: "f001", Created: "2026-10-16T00:00:00Z",
		SceneMs: clips[len(clips)-1].DurMs / 2,
		ClipID:  clips[len(clips)-1].ClipID,
		LineID:  "l09", Kind: "logic",
		Text:   "开门之前缺一个\"察觉异常\"的镜头，否则摄影师的反应没有依据。",
		Status: "open",
	}}
	if err := os.MkdirAll(filepath.Join(dir, "scenes", "demo-door"), 0o755); err != nil {
		return err
	}
	st := &Studio{}
	if err := st.saveScene(filepath.Join(dir, "scenes", "demo-door"), sc, false); err != nil {
		return err
	}
	for _, w := range warns {
		fmt.Println("warn:", w)
	}
	fmt.Println("workspace:", dir)
	fmt.Println("scene:", filepath.Join(dir, "scenes", "demo-door"), "-", len(clips), "clips,", len(sc.Script.Lines), "lines")

	// 第二个场景：配音链路演示。用 vendored fixture（testdata/dub-example）
	// 的台本+take.wav+asr-raw.json 跑真 Reconcile/Render——不依赖 funasr，
	// ASR 结果是离线样例，但 timeline→渲染→manifest 全是真路径。
	if err := genVoiceDemo(dir, sc.SceneID); err != nil {
		fmt.Println("voice demo 跳过:", err)
	}
	return nil
}

// genVoiceDemo 生成"配音链路示例"场景：fixture 台本 + take 录音 + ASR 对齐
// + DSP 渲染产物全部就位，打开即可在配音面板看到对齐表和 wav。
func genVoiceDemo(ws, _ string) error {
	fix, err := filepath.Abs(filepath.Join("testdata", "dub-example"))
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(fix, "take.wav")); err != nil {
		return fmt.Errorf("fixture 缺 take.wav: %w", err)
	}
	sc, err := dubbing.LoadScript(filepath.Join(fix, "script.json"))
	if err != nil {
		return err
	}
	scene := &Scene{
		Version: SceneVersion, SceneID: "demo-voice", Title: "配音链路示例（带真音频）",
		Roles: []Role{
			{ID: "narrator", Name: "旁白", Color: "#4a90d9",
				Voice: &dubbing.VoiceProfile{Backend: dubbing.VoiceBackendDSP, Preset: "normal"}},
			{ID: "character-a", Name: "角色A", Color: "#d9634a",
				Voice: &dubbing.VoiceProfile{Backend: dubbing.VoiceBackendDSP, Preset: "natural-woman"}},
		},
		Script: *sc,
	}
	clips, _, err := GenDraft(scene, nil)
	if err != nil {
		return err
	}
	scene.Clips = clips
	for _, c := range clips {
		scene.Order = append(scene.Order, c.ClipID)
	}
	sceneDir := filepath.Join(ws, "scenes", "demo-voice")
	if err := os.MkdirAll(sceneDir, 0o755); err != nil {
		return err
	}
	st := &Studio{}
	if err := st.saveScene(sceneDir, scene, false); err != nil {
		return err
	}
	// take.wav → audio/rec/
	recDir := filepath.Join(ws, "audio", "rec")
	if err := os.MkdirAll(recDir, 0o755); err != nil {
		return err
	}
	take := filepath.Join(recDir, "take-demo.wav")
	src, err := os.ReadFile(filepath.Join(fix, "take.wav"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(take, src, 0o644); err != nil {
		return err
	}
	// asr-raw.json + reconcile → timeline.json（真实 Reconcile，不调 funasr）
	asrDoc, err := dubbing.LoadASRDoc(filepath.Join(fix, "asr-raw.json"))
	if err != nil {
		return err
	}
	sess := filepath.Join(ws, "audio", "dubbing", "demo-session")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		return err
	}
	tl, err := dubbing.Reconcile(sc, asrDoc, dubbing.DefaultReconcileOptions())
	if err != nil {
		return err
	}
	tl.Audio = take
	if err := dubbing.SaveJSON(filepath.Join(sess, "timeline.json"), tl); err != nil {
		return err
	}
	asrBytes, _ := os.ReadFile(filepath.Join(fix, "asr-raw.json"))
	if err := os.WriteFile(filepath.Join(sess, "asr-raw.json"), asrBytes, 0o644); err != nil {
		return err
	}
	// voices.json + Render（真 DSP 渲染）
	voices := dubbing.Voices{Version: 1, Voices: map[string]dubbing.VoiceProfile{}}
	for _, r := range scene.Roles {
		if r.Voice != nil {
			voices.Voices[r.ID] = *r.Voice
		}
	}
	vp := filepath.Join(ws, "audio", "voices-demo-voice.json")
	vb, _ := json.MarshalIndent(voices, "", "  ")
	if err := dubbing.WriteFileAtomic(vp, vb, 0o644); err != nil {
		return err
	}
	m, err := dubbing.Render(dubbing.RenderOptions{
		TimelinePath:    filepath.Join(sess, "timeline.json"),
		AudioPath:       take,
		VoicesPath:      vp,
		OutDir:          filepath.Join(sess, "rendered"),
		WriteSrc:        true,
		AllowUnreviewed: true,
	})
	if err != nil {
		return err
	}
	// 回写音频状态（带输入指纹，供"是否过期"判断）
	tlPath := filepath.Join(sess, "timeline.json")
	scene.Audio = AudioState{Take: take, TakeSig: fileSig(take), Session: sess,
		Timeline: tlPath, Rendered: filepath.Join(sess, "rendered")}
	scene.Audio.TimelineSig = scene.expectedTimelineSig()
	scene.Audio.RenderedSig = scene.expectedRenderedSig()
	if err := st.saveScene(sceneDir, scene, false); err != nil {
		return err
	}
	fmt.Println("voice scene:", sceneDir, "- manifest status:", m.Status,
		"lines:", len(m.Lines), "skipped:", len(m.Skipped))
	return nil
}
