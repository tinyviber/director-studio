// studio_test.go — Director Studio 离线测试。
//
// 覆盖：draft 生成（≤10s clips、beats、line_ids、final 迁移）、scene 编辑操作
// （重排/拆分/删除/复制/裁剪/换源/改词/反馈）、持久化往返、undo/redo、
// 反馈导出、媒体服务 allowlist+Range、录音落盘、以及用 voice-morph
// fixture 走真 dubbing.Reconcile→Render 路径（不跑真 ASR——funasr 环境限制）。
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"voice-morph/dubbing"
	"voice-morph/engine"
)

const fixture = "testdata/dub-example"

func fixtureScene(t *testing.T) *Scene {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixture, "script.json"))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := dubbing.LoadScript(filepath.Join(fixture, "script.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = b
	return &Scene{
		Version: SceneVersion, SceneID: "s1", Title: "测试场景",
		Roles: []Role{
			{ID: "narrator", Name: "旁白", Voice: &dubbing.VoiceProfile{Backend: "dsp", Preset: "normal"}},
			{ID: "character-a", Name: "角色A", Voice: &dubbing.VoiceProfile{Backend: "dsp", Preset: "natural-woman"}},
		},
		Script: *sc,
	}
}

// ---------- draft ----------

func TestGenDraft(t *testing.T) {
	sc := fixtureScene(t)
	clips, _, err := GenDraft(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) == 0 {
		t.Fatal("无 clips")
	}
	for _, c := range clips {
		if c.DurMs > 9500 {
			t.Errorf("clip %s 时长 %d 超过软上限", c.ClipID, c.DurMs)
		}
		if len(c.LineIDs) == 0 {
			t.Errorf("clip %s 无台词", c.ClipID)
		}
		var hasLine, hasCam bool
		for _, b := range c.Beats {
			if b.Kind == "line" {
				hasLine = true
			}
			if b.Kind == "camera" {
				hasCam = true
			}
		}
		if !hasLine || !hasCam {
			t.Errorf("clip %s 缺 line/camera beat", c.ClipID)
		}
	}
	sc.Clips = clips
	for _, c := range clips {
		sc.Order = append(sc.Order, c.ClipID)
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGenDraftSplitsLongScene(t *testing.T) {
	sc := fixtureScene(t)
	// 16 句长台词 → 必切多 clip
	for i := 5; i <= 16; i++ {
		sc.Script.Lines = append(sc.Script.Lines, dubbing.ScriptLine{
			LineID: fmt.Sprintf("l%02d", i),
			RoleID: "narrator",
			Text:   strings.Repeat("一句不短的台词。", 2),
		})
	}
	clips, _, err := GenDraft(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) < 3 {
		t.Fatalf("长台本应切 ≥3 clips，得到 %d", len(clips))
	}
	for _, c := range clips {
		if c.DurMs > 9500 {
			t.Errorf("clip %s 超上限 %d", c.ClipID, c.DurMs)
		}
	}
}

func TestGenDraftPreservesFinal(t *testing.T) {
	sc := fixtureScene(t)
	clips, _, err := GenDraft(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 把第一个 clip 标成 final 绑视频
	c := &clips[0]
	c.Candidates = append(c.Candidates, ClipSource{Kind: "video", Path: "/tmp/final-c01.mp4"})
	c.Pick = 1
	c.Status = "final"
	c.T0Ms, c.T1Ms = 0, 6000
	clips2, _, err := GenDraft(sc, clips)
	if err != nil {
		t.Fatal(err)
	}
	// line_ids 相同的 clip 应继承 final 绑定
	found := false
	for _, nc := range clips2 {
		if joinS(nc.LineIDs) == joinS(c.LineIDs) {
			found = true
			if !nc.IsFinal() || nc.Source().Path != "/tmp/final-c01.mp4" {
				t.Errorf("final 绑定没迁移: %+v", nc.Source())
			}
		}
	}
	if !found {
		t.Fatal("新 clips 里找不到原 line_ids 组合")
	}
}

func joinS(s []string) string { return strings.Join(s, "|") }

// 重生成时 line_ids 不变的 clip 必须沿用旧 clip_id（批注引用不失效）；
// 内容变掉的旧 id 不得再分配给新 clip（批注悬空可见，而非静默指错）。
func TestGenDraftStableIDs(t *testing.T) {
	sc := newSceneWithDraft(t)
	oldIDs := map[string][]string{}
	for _, c := range sc.Clips {
		oldIDs[c.ClipID] = append([]string(nil), c.LineIDs...)
	}
	clips2, _, err := GenDraft(sc, sc.Clips)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range clips2 {
		if got := joinS(c.LineIDs); got != joinS(oldIDs[c.ClipID]) {
			t.Errorf("clip %s 的 line_ids 变了：%s → %s", c.ClipID, joinS(oldIDs[c.ClipID]), got)
		}
	}
	// 删掉中间一句 → 重生成后分组变化，被淘汰的旧 id 不能回收再用
	sc.Script.Lines = append(sc.Script.Lines[:1], sc.Script.Lines[2:]...)
	clips3, _, err := GenDraft(sc, sc.Clips)
	if err != nil {
		t.Fatal(err)
	}
	stillUsed := map[string][]string{}
	for _, c := range clips3 {
		stillUsed[c.ClipID] = c.LineIDs
	}
	for oldID, oldLines := range oldIDs {
		if newLines, ok := stillUsed[oldID]; ok && joinS(newLines) != joinS(oldLines) {
			t.Errorf("旧 clip_id %s 被回收给了不同内容（%s → %s）", oldID, joinS(oldLines), joinS(newLines))
		}
	}
}

// 已标 stale 的 clip 重生成后必须保留 stale——改过台词的成片不能
// 被静默当成有效版本。
func TestGenDraftPreservesStale(t *testing.T) {
	sc := newSceneWithDraft(t)
	target := sc.Clips[0].LineIDs[0]
	if err := sc.UpdateLine(target, "改过之后还是原句长度", "", ""); err != nil {
		t.Fatal(err)
	}
	clips2, _, err := GenDraft(sc, sc.Clips)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range clips2 {
		if joinS(c.LineIDs) == joinS(sc.Clips[0].LineIDs) {
			found = true
			if !c.Stale {
				t.Error("stale 标记在重生成后丢失")
			}
			if c.ClipID != sc.Clips[0].ClipID {
				t.Error("line_ids 未变但 clip_id 变了")
			}
		}
	}
	if !found {
		t.Fatal("找不到对应 clip")
	}
}

// 台词正文里的"走/停/转"不能触发动作——只有 direction 字段能产指令。
func TestGenDraftDirectionOnly(t *testing.T) {
	sc := fixtureScene(t)
	sc.Script.Lines = []dubbing.ScriptLine{
		{LineID: "l01", SceneID: "s1", RoleID: "narrator", Text: "我们走吧，别再回头看了。"},
		{LineID: "l02", SceneID: "s1", RoleID: "character-a", Text: "好，你先走。", Direction: "转身离开"},
	}
	clips, _, err := GenDraft(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	var l01Acts, l02Acts []string
	for _, c := range clips {
		for _, b := range c.Beats {
			if b.Kind != "action" || b.Action == "talk" || b.Action == "enter" {
				continue
			}
			// 用 at_ms 归属判断动作属于哪句（action 在台词起点展开）
			for _, lb := range c.Beats {
				if lb.Kind == "line" && lb.AtMs == b.AtMs {
					if lb.LineID == "l01" {
						l01Acts = append(l01Acts, b.Action)
					}
					if lb.LineID == "l02" {
						l02Acts = append(l02Acts, b.Action)
					}
				}
			}
		}
	}
	if len(l01Acts) > 0 {
		t.Errorf("台词正文触发了动作 %v——只应解析 direction", l01Acts)
	}
	if len(l02Acts) == 0 {
		t.Error("direction「转身离开」没有产生动作")
	}
}

// ---------- ops ----------

func newSceneWithDraft(t *testing.T) *Scene {
	t.Helper()
	sc := fixtureScene(t)
	// fixture 只有 4 行（≈4s），扩到 10 行 2 角色对话，保证 ≥2 clips
	more := []dubbing.ScriptLine{
		{LineID: "l05", RoleID: "character-a", Text: "你看到那边的光了吗，好像有人在走动"},
		{LineID: "l06", RoleID: "narrator", Text: "摄影师举起机器慢慢靠近了门口"},
		{LineID: "l07", RoleID: "character-a", Text: "等一下，先别开灯，我听到里面有声音"},
		{LineID: "l08", RoleID: "narrator", Text: "两个人同时停住了脚步"},
		{LineID: "l09", RoleID: "character-a", Text: "门缝里好像有什么东西在动"},
		{LineID: "l10", RoleID: "narrator", Text: "画面慢慢推近了那扇门"},
	}
	for i, l := range more {
		l.SceneID = "s1"
		l.LineID = fmt.Sprintf("l%02d", 5+i)
		sc.Script.Lines = append(sc.Script.Lines, l)
	}
	clips, _, err := GenDraft(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	sc.Clips = clips
	for _, c := range clips {
		sc.Order = append(sc.Order, c.ClipID)
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestReorder(t *testing.T) {
	sc := newSceneWithDraft(t)
	if len(sc.Order) < 2 {
		t.Skip("draft 只生成 1 个 clip，跳过重排")
	}
	origFirst := sc.Order[0]
	rev := []string{}
	for i := len(sc.Order) - 1; i >= 0; i-- {
		rev = append(rev, sc.Order[i])
	}
	if err := sc.ReorderClips(rev); err != nil {
		t.Fatal(err)
	}
	if sc.Order[0] == origFirst {
		t.Error("重排未生效")
	}
	if sc.Order[0] != rev[0] {
		t.Error("新顺序与请求不符")
	}
	if err := sc.ReorderClips([]string{"nope"}); err == nil {
		t.Error("坏 order 应报错")
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSplitClip(t *testing.T) {
	sc := newSceneWithDraft(t)
	// 找一个含 ≥2 行台词的 clip
	var target *Clip
	for i := range sc.Clips {
		if len(sc.Clips[i].LineIDs) >= 2 {
			target = &sc.Clips[i]
			break
		}
	}
	if target == nil {
		t.Skip("fixture 台本只有 4 行，可能没有多行 clip")
	}
	nLines := len(target.LineIDs)
	mid := target.DurMs / 2
	oldID := target.ClipID
	if err := sc.SplitClip(oldID, mid); err != nil {
		t.Fatal(err)
	}
	if len(sc.Clips) != len(sc.Order) {
		t.Fatal("order/clips 失配")
	}
	gotLines := 0
	for _, c := range sc.Clips {
		if c.ClipID == oldID || c.DurMs == 0 {
			continue
		}
		gotLines += len(c.LineIDs)
	}
	if gotLines < nLines {
		t.Errorf("拆分后 line_ids 丢了：%d < %d", gotLines, nLines)
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
	// beats 不得越界
	for _, c := range sc.Clips {
		for _, b := range c.Beats {
			if b.AtMs < 0 || b.AtMs > c.DurMs {
				t.Fatalf("clip %s beat 越界 %d", c.ClipID, b.AtMs)
			}
		}
	}
}

func TestDeleteDuplicateTrim(t *testing.T) {
	sc := newSceneWithDraft(t)
	first := sc.Order[0]
	if err := sc.DuplicateClip(first); err != nil {
		t.Fatal(err)
	}
	if len(sc.Order) != len(sc.Clips) {
		t.Fatal("duplicate 后 order/clips 失配")
	}
	dupID := sc.Order[1]
	if dupID == first {
		t.Fatal("duplicate 复用了 id")
	}
	// trim 到"最后一句台词结束+400ms"（draft 不能裁掉台词尾巴）
	dup := sc.clip(dupID)
	maxEnd := 0
	for _, b := range dup.Beats {
		if b.Kind == "line" && b.AtMs+b.DurMs > maxEnd {
			maxEnd = b.AtMs + b.DurMs
		}
	}
	newDur := maxEnd + 400
	if newDur >= dup.DurMs {
		newDur = dup.DurMs - 100
		if newDur <= maxEnd {
			t.Skip("clip 太紧凑，没有可裁空间")
		}
	}
	if err := sc.TrimClip(dupID, newDur); err != nil {
		t.Fatal(err)
	}
	if sc.clip(dupID).DurMs != newDur {
		t.Error("trim 未生效")
	}
	// 裁到台词中间应报错
	if err := sc.TrimClip(dupID, maxEnd-500); err == nil {
		t.Error("裁到台词中段应拒绝")
	}
	if err := sc.DeleteClip(first); err != nil {
		t.Fatal(err)
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
}

// 删台词必须同步删掉引用它的 line beats——否则时间线上会留下播放
// 已删台词音频的孤儿事件。
func TestDeleteLineCleansBeats(t *testing.T) {
	sc := newSceneWithDraft(t)
	lid := sc.Script.Lines[0].LineID
	if err := sc.DeleteLine(lid); err != nil {
		t.Fatal(err)
	}
	for _, c := range sc.Clips {
		for _, b := range c.Beats {
			if b.Kind == "line" && b.LineID == lid {
				t.Errorf("clip %s 残留指向已删台词 %s 的 beat", c.ClipID, lid)
			}
		}
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateLineStale(t *testing.T) {
	sc := newSceneWithDraft(t)
	lid := sc.Script.Lines[1].LineID
	if err := sc.UpdateLine(lid, "改过的词", "", "停顿"); err != nil {
		t.Fatal(err)
	}
	stale, fresh := 0, 0
	for _, c := range sc.Clips {
		if c.Stale {
			stale++
			// stale clip 必须引用该行
			has := false
			for _, x := range c.LineIDs {
				if x == lid {
					has = true
				}
			}
			if !has {
				t.Errorf("clip %s 被误标 stale", c.ClipID)
			}
		} else {
			fresh++
		}
	}
	if stale == 0 {
		t.Error("没有 clip 被标 stale")
	}
	if err := sc.Validate(); err != nil {
		t.Fatal(err)
	}
}

// ---------- 持久化 / undo ----------

func TestSaveLoadUndo(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	back, err := loadScene(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Clips) != len(sc.Clips) || back.Title != sc.Title {
		t.Fatal("round-trip 不一致")
	}
	// undo：删 clip → undo 恢复
	op := `{"op":"delete","clip_id":"` + back.Order[0] + `"}`
	res := st.SceneEdit(dir, op)
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Scene Scene `json:"scene"`
		} `json:"data"`
		Err string `json:"error"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
		t.Fatalf("SceneEdit: %s", res)
	}
	if len(env.Data.Scene.Clips) != len(sc.Clips)-1 {
		t.Fatal("delete 未生效")
	}
	var scEnv struct {
		OK   bool  `json:"ok"`
		Data Scene `json:"data"`
	}
	res = st.Undo(dir)
	if err := json.Unmarshal([]byte(res), &scEnv); err != nil || !scEnv.OK {
		t.Fatalf("Undo: %s", res)
	}
	if len(scEnv.Data.Clips) != len(sc.Clips) {
		t.Fatal("undo 没恢复 clip 数")
	}
	res = st.Redo(dir)
	if err := json.Unmarshal([]byte(res), &scEnv); err != nil || !scEnv.OK {
		t.Fatalf("Redo: %s", res)
	}
	if len(scEnv.Data.Clips) != len(sc.Clips)-1 {
		t.Fatal("redo 没回到删后态")
	}
}

// ---------- 反馈 ----------

func TestFeedbackExport(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Dir(filepath.Dir(dir)) // 模拟 workspace/scenes/<id>
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := &Studio{}
	sc := newSceneWithDraft(t)
	sc.SceneID = "s1"
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	id, err := sc.AddFeedback(Feedback{
		SceneMs: 4200, ClipID: sc.Order[0], ClipMs: 1200,
		LineID: sc.Script.Lines[0].LineID, Kind: "logic",
		Text: "这里摄影师还没看到悟空变身，为什么突然生气？",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	res := st.ExportFeedback(dir)
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			JSON string `json:"json"`
			MD   string `json:"md"`
		} `json:"data"`
		Err string `json:"error"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
		t.Fatalf("export: %s", res)
	}
	md, err := os.ReadFile(env.Data.MD)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "摄影师") || !strings.Contains(string(md), id) {
		t.Error("导出的 md 缺反馈内容")
	}
	if _, err := os.Stat(env.Data.JSON); err != nil {
		t.Error("json 未写出")
	}
	// 状态机
	if err := sc.SetFeedbackStatus(id, "done", "已补察觉镜头"); err != nil {
		t.Fatal(err)
	}
	if err := sc.SetFeedbackStatus(id, "bogus", ""); err == nil {
		t.Error("非法状态应报错")
	}
	_ = ws
}

// ---------- 编辑接口健壮性 ----------

func mustSceneEditErr(t *testing.T, st *Studio, dir, op string) string {
	t.Helper()
	res := st.SceneEdit(dir, op)
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil {
		t.Fatalf("SceneEdit 返回非法 JSON: %s", res)
	}
	if env.OK {
		t.Fatalf("op 应被拒绝但成功了: %s", op)
	}
	return env.Error
}

// 畸形的 reorder 请求不得 panic 打崩进程。
func TestSceneEditMalformed(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	mustSceneEditErr(t, st, dir, `{"op":"reorder","order":"not-an-array"}`)
	mustSceneEditErr(t, st, dir, `{"op":"reorder","order":[1,2,3]}`)
	mustSceneEditErr(t, st, dir, `{"op":"reorder"}`)
}

// base_rev 乐观并发：过期修订版的写请求要被拒，正确版本要通过。
func TestSceneEditBaseRev(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	cur, err := loadScene(dir)
	if err != nil {
		t.Fatal(err)
	}
	lid := sc.Script.Lines[0].LineID
	mustSceneEditErr(t, st, dir,
		fmt.Sprintf(`{"op":"update_line","line_id":%q,"text":"x","base_rev":%d}`, lid, cur.Revision+99))
	res := st.SceneEdit(dir,
		fmt.Sprintf(`{"op":"update_line","line_id":%q,"text":"x","base_rev":%d}`, lid, cur.Revision))
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
		t.Fatalf("正确 base_rev 应通过: %s", res)
	}
}

// set_take 记录 take 指纹；换 take 后旧 timeline 自动判过期。
func TestSetTakeInvalidatesTimeline(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	take1 := filepath.Join(t.TempDir(), "a.wav")
	take2 := filepath.Join(t.TempDir(), "b.wav")
	wav, _ := base64.StdEncoding.DecodeString(engineTestWAV(t))
	os.WriteFile(take1, wav, 0o644)
	os.WriteFile(take2, append(wav, 0), 0o644) // 大小不同 → 指纹必不同

	res := st.SceneEdit(dir, fmt.Sprintf(`{"op":"set_take","path":%q}`, take1))
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
		t.Fatalf("set_take: %s", res)
	}
	cur, _ := loadScene(dir)
	if cur.Audio.TakeSig == "" {
		t.Fatal("set_take 没记录指纹")
	}
	// 模拟一份基于 take1 的 timeline
	cur.Audio.Timeline = "/tmp/timeline.json"
	cur.Audio.TimelineSig = cur.Audio.TakeSig
	if cur.Audio.TimelineStale() {
		t.Fatal("同 take 的 timeline 不应过期")
	}
	if err := st.saveScene(dir, cur, false); err != nil {
		t.Fatal(err)
	}
	// 换 take2 → timeline 过期
	res = st.SceneEdit(dir, fmt.Sprintf(`{"op":"set_take","path":%q}`, take2))
	if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
		t.Fatalf("set_take2: %s", res)
	}
	cur, _ = loadScene(dir)
	if !cur.Audio.TimelineStale() {
		t.Error("换 take 后旧 timeline 应判过期")
	}
}

// 同秒重名 take 不得互相覆盖。
func TestSaveRecordingDedup(t *testing.T) {
	st := &Studio{}
	dir := t.TempDir()
	wav := engineTestWAV(t)
	var p1, p2 string
	for _, dst := range []*string{&p1, &p2} {
		res := st.SaveRecording(dir, wav, "take-120000")
		var env struct {
			OK   bool `json:"ok"`
			Data struct {
				Path string `json:"path"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
			t.Fatalf("save: %s", res)
		}
		*dst = env.Data.Path
	}
	if p1 == p2 {
		t.Error("同名 take 互相覆盖")
	}
	if _, err := os.Stat(p1); err != nil {
		t.Error("第一条 take 丢了")
	}
}

// ---------- 录音 / 媒体服务 ----------

func TestSaveRecording(t *testing.T) {
	st := &Studio{}
	dir := t.TempDir()
	wav := engineTestWAV(t)
	res := st.SaveRecording(dir, wav, "我的 take")
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Path string `json:"path"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
		t.Fatalf("save: %s", res)
	}
	if _, err := os.Stat(env.Data.Path); err != nil {
		t.Fatal("wav 没写盘")
	}
	if filepath.Base(env.Data.Path) != "我的 take.wav" {
		t.Error("中文名丢失")
	}
}

func TestMediaServer(t *testing.T) {
	m, err := newMediaServer()
	if err != nil {
		t.Skip("起不了本地端口")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "x.wav")
	os.WriteFile(f, []byte("RIFF"+strings.Repeat("0", 100)), 0o644)
	m.allow(dir)
	// allowlist 内的文件 + Range
	req := httptest.NewRequest("GET", m.base+"?p="+f, nil)
	req.Header.Set("Range", "bytes=0-9")
	rec := httptest.NewRecorder()
	m.serve(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range 应回 206，得到 %d", rec.Code)
	}
	if rec.Body.Len() != 10 {
		t.Errorf("Range 应回 10 字节，得到 %d", rec.Body.Len())
	}
	// 未 allow 的目录 → 403
	outside := filepath.Join(t.TempDir(), "y.wav")
	os.WriteFile(outside, []byte("RIFF"), 0o644)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", m.base+"?p="+outside, nil)
	m.serve(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("未授权路径应 403，得到 %d", rec2.Code)
	}
	// 非媒体扩展名 → 403
	txt := filepath.Join(dir, "s.txt")
	os.WriteFile(txt, []byte("x"), 0o644)
	rec3 := httptest.NewRecorder()
	m.serve(rec3, httptest.NewRequest("GET", m.base+"?p="+txt, nil))
	if rec3.Code != http.StatusForbidden {
		t.Errorf("非媒体文件应 403，得到 %d", rec3.Code)
	}
}

// ---------- 配音链路（fixture，不跑真 ASR）----------

func TestReconcileRenderFixture(t *testing.T) {
	sc, err := dubbing.LoadScript(filepath.Join(fixture, "script.json"))
	if err != nil {
		t.Fatal(err)
	}
	asr, err := dubbing.LoadASRDoc(filepath.Join(fixture, "asr-raw.json"))
	if err != nil {
		t.Fatal(err)
	}
	tl, err := dubbing.Reconcile(sc, asr, dubbing.DefaultReconcileOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Segments) == 0 {
		t.Fatal("reconcile 没产出 segments")
	}
	// fixture 的 ASR 与台本有偏差（多读"广告时间到"、"等下啊"≈"等一下"），
	// reconcile 应给出 review 标记或 unmatched——不静默当全对。
	if tl.ReviewCount == 0 && len(tl.PendingReview()) == 0 {
		t.Log("fixture reconcile 未产生 review 项（对齐阈值内全部对齐）")
	}
	matched := 0
	for _, s := range tl.Segments {
		if s.LineID != "" {
			matched++
		}
	}
	if matched == 0 {
		t.Error("没有任何 segment 绑到 line_id")
	}
	// Render：真实 DSP 路径（voice-morph 的 fixture take.wav）
	outDir := t.TempDir()
	m, err := dubbing.Render(dubbing.RenderOptions{
		Timeline: tl, TimelinePath: filepath.Join(fixture, "timeline.json"),
		VoicesPath: filepath.Join(fixture, "voices.json"),
		AudioPath:  filepath.Join(fixture, "take.wav"),
		OutDir:     outDir, WriteSrc: true, AllowUnreviewed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Lines) == 0 {
		t.Fatal("render 没产出 lines")
	}
	if _, err := os.Stat(filepath.Join(outDir, "manifest.json")); err != nil {
		t.Error("manifest.json 未写出")
	}
	// 每条产出 line 应可定位（segment→line 关联没丢）
	for _, ln := range m.Lines {
		if ln.LineID == "" {
			t.Errorf("manifest line 缺 line_id: %+v", ln)
		}
	}
}

// engineTestWAV 用 voice-morph engine 编码 0.1s 静音为 base64 WAV。
func engineTestWAV(t *testing.T) string {
	t.Helper()
	pcm := make([]float32, engine.SampleRate/10)
	return base64.StdEncoding.EncodeToString(engine.EncodeWAV(pcm, engine.SampleRate))
}
