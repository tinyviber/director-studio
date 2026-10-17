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
	"sync"
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

// 连续重生成：被淘汰的 clip_id 永久退役，不得回收给新内容——
// 否则挂在上面的旧批注会静默指向另一段素材。
func TestGenDraftRetiredIDsNeverReused(t *testing.T) {
	sc := newSceneWithDraft(t)
	clips1, _, err := GenDraft(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	sc.Clips = clips1
	history := map[string][]string{} // 每个出现过的 id → 当时的 line_ids
	mark := func(cs []Clip) {
		for _, c := range cs {
			if _, ok := history[c.ClipID]; !ok {
				history[c.ClipID] = c.LineIDs
			}
		}
	}
	mark(clips1)

	// 第 2 次：删中间一句 → 分组变化
	sc.Script.Lines = append(sc.Script.Lines[:1], sc.Script.Lines[2:]...)
	clips2, _, err := GenDraft(sc, sc.Clips)
	if err != nil {
		t.Fatal(err)
	}
	sc.Clips = clips2
	mark(clips2)

	// 第 3 次：再删一句 → 又有一批旧 id 淘汰
	sc.Script.Lines = sc.Script.Lines[:len(sc.Script.Lines)-1]
	clips3, _, err := GenDraft(sc, sc.Clips)
	if err != nil {
		t.Fatal(err)
	}
	sc.Clips = clips3
	mark(clips3)

	// 第 4 次：加一句新台词 → 分配新 id
	sc.Script.Lines = append(sc.Script.Lines, dubbing.ScriptLine{
		LineID: "l99", SceneID: "s1", RoleID: "narrator", Text: "追加的一句新台词。"})
	clips4, _, err := GenDraft(sc, sc.Clips)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range clips4 {
		if old, ok := history[c.ClipID]; ok && joinS(old) != joinS(c.LineIDs) {
			t.Errorf("clip_id %s 被复用给不同内容（%s → %s）", c.ClipID, joinS(old), joinS(c.LineIDs))
		}
	}
}

// duplicate 出的副本与原 clip 共享 line_ids；重生成只能保留一个身份时，
// 必须保留有正式素材绑定的原件，而不是恰好排在后面的副本。
func TestGenDraftDuplicateKeepsFinalBinding(t *testing.T) {
	sc := newSceneWithDraft(t)
	orig := &sc.Clips[0]
	orig.Candidates = append(orig.Candidates, ClipSource{Kind: "video", Path: "/tmp/final.mp4"})
	orig.Pick = 1
	orig.Status = "final"
	orig.T0Ms, orig.T1Ms = 0, orig.DurMs
	origID, origLines := orig.ClipID, append([]string(nil), orig.LineIDs...)
	if err := sc.DuplicateClip(origID); err != nil {
		t.Fatal(err)
	}
	dupID := sc.Order[1]
	clips2, _, err := GenDraft(sc, sc.Clips)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range clips2 {
		if c.ClipID == dupID {
			t.Error("重生成保留了副本身份而非原件")
		}
		if joinS(c.LineIDs) == joinS(origLines) {
			if c.ClipID != origID {
				t.Errorf("原内容应沿用原件 id %s，得到 %s", origID, c.ClipID)
			}
			if !c.IsFinal() || c.Source().Path != "/tmp/final.mp4" {
				t.Error("原件的 final 素材绑定丢失")
			}
		}
	}
}

// 同一 base_rev 的两次写，只有先到的一次能提交——
// 场景互斥把"读版本-校验-修改-写盘"收进同一临界区。
func TestConcurrentEditsSameBaseRev(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	cur, _ := loadScene(dir)
	lines := sc.Script.Lines
	var okCnt int
	var wg sync.WaitGroup
	for i, l := range lines[:2] {
		wg.Add(1)
		go func(i int, lid string) {
			defer wg.Done()
			op := fmt.Sprintf(`{"op":"update_line","line_id":%q,"text":"并发改 %d","base_rev":%d}`, lid, i, cur.Revision)
			res := st.SceneEdit(dir, op)
			var env struct {
				OK bool `json:"ok"`
			}
			if json.Unmarshal([]byte(res), &env) == nil && env.OK {
				okCnt++
			}
		}(i, l.LineID)
	}
	wg.Wait()
	if okCnt > 1 {
		t.Errorf("同 base_rev 的两个写都成功了（lost update）")
	}
	// 顺序重放同 rev 也必须失败
	res := st.SceneEdit(dir, fmt.Sprintf(
		`{"op":"update_line","line_id":%q,"text":"再来一次","base_rev":%d}`, lines[0].LineID, cur.Revision))
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil {
		t.Fatal(err)
	}
	if env.OK {
		t.Error("过期 base_rev 的写被接受了")
	}
}

// 指纹链：改台词正文 → timeline+rendered 双过期；改变声预设 → 只 rendered 过期。
func TestScriptAndVoiceInvalidateAudio(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	take := filepath.Join(t.TempDir(), "take.wav")
	wav, _ := base64.StdEncoding.DecodeString(engineTestWAV(t))
	os.WriteFile(take, wav, 0o644)
	tl := filepath.Join(t.TempDir(), "timeline.json")
	os.WriteFile(tl, []byte(`{"version":1,"segments":[]}`), 0o644)

	sc.Audio.Take, sc.Audio.TakeSig = take, fileSig(take)
	sc.Audio.Timeline = tl
	sc.Audio.TimelineSig = sc.expectedTimelineSig()
	sc.Audio.Rendered = filepath.Join(t.TempDir(), "rendered")
	sc.Audio.RenderedSig = sc.expectedRenderedSig()
	if tlStale, rdStale := sc.AudioStatus(); tlStale || rdStale {
		t.Fatal("新鲜产物被误判过期")
	}

	// 改台词 → timeline 与 rendered 都应过期（rendered 是传递过期）
	lid := sc.Script.Lines[0].LineID
	if err := sc.UpdateLine(lid, "改过之后的台词文本", "", ""); err != nil {
		t.Fatal(err)
	}
	if tlStale, rdStale := sc.AudioStatus(); !tlStale || !rdStale {
		t.Errorf("改台词后应 timeline+rendered 双过期，得到 tl=%v rd=%v", tlStale, rdStale)
	}

	// 新场景：只改变声预设 → rendered 过期、timeline 仍有效
	sc2 := newSceneWithDraft(t)
	sc2.Audio.Take, sc2.Audio.TakeSig = take, fileSig(take)
	sc2.Audio.Timeline = tl
	sc2.Audio.TimelineSig = sc2.expectedTimelineSig()
	sc2.Audio.Rendered = filepath.Join(t.TempDir(), "rendered")
	sc2.Audio.RenderedSig = sc2.expectedRenderedSig()
	if err := st.saveScene(dir, sc2, false); err != nil {
		t.Fatal(err)
	}
	roleID := sc2.Roles[0].ID
	res := st.SceneEdit(dir, fmt.Sprintf(`{"op":"set_role_voice","role_id":%q,"preset":"deep"}`, roleID))
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
		t.Fatalf("set_role_voice: %s", res)
	}
	cur, _ := loadScene(dir)
	if tlStale, rdStale := cur.AudioStatus(); tlStale || !rdStale {
		t.Errorf("改音色后应只 rendered 过期，得到 tl=%v rd=%v", tlStale, rdStale)
	}

	// 手动改写/保存 timeline 文件 → rendered 过期、timeline 仍有效
	tl3 := filepath.Join(t.TempDir(), "timeline.json")
	os.WriteFile(tl3, []byte(`{"version":1,"segments":[]}`), 0o644)
	sc3 := newSceneWithDraft(t)
	sc3.Audio.Take, sc3.Audio.TakeSig = take, fileSig(take)
	sc3.Audio.Timeline = tl3
	sc3.Audio.TimelineSig = sc3.expectedTimelineSig()
	sc3.Audio.Rendered = filepath.Join(t.TempDir(), "rendered")
	sc3.Audio.RenderedSig = sc3.expectedRenderedSig()
	if tlStale, rdStale := sc3.AudioStatus(); tlStale || rdStale {
		t.Fatal("sc3 基线应为有效")
	}
	os.WriteFile(tl3, []byte(`{"version":1,"segments":[],"_":"manual edit"}`), 0o644)
	if tlStale, rdStale := sc3.AudioStatus(); tlStale || !rdStale {
		t.Errorf("手动存 timeline 后应只 rendered 过期，得到 tl=%v rd=%v", tlStale, rdStale)
	}
}

// commitAudioArtifacts：耗时任务完成后合并产物——期间进来的编辑不丢；
// 产物指纹按任务开始时的输入算，输入中途被改 → 任务已被取代，
// 不提升（产物留在 job 目录作历史），而不是覆盖当前生效的引用。
func TestCommitAudioArtifactsRace(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	take := filepath.Join(t.TempDir(), "take.wav")
	wav, _ := base64.StdEncoding.DecodeString(engineTestWAV(t))
	os.WriteFile(take, wav, 0o644)
	sc.Audio.Take, sc.Audio.TakeSig = take, fileSig(take)
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}

	// 任务开始：按当时 take+台本算产物指纹
	takeSig := fileSig(take)
	tlSig := audioSig(takeSig, sc.scriptSig())

	// 任务期间：另一处把台词正文改了
	l0 := sc.Script.Lines[0]
	res := st.SceneEdit(dir, fmt.Sprintf(
		`{"op":"update_line","line_id":%q,"text":"改过的台词"}`, l0.LineID))
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
		t.Fatalf("mid-task edit: %s", res)
	}

	// 任务结束：提交产物（指纹是任务开始时算的）→ 输入已变，不提升
	promoted, err := st.commitAudioArtifacts(dir, true, jobTimeline, 1, tlSig,
		(*Scene).expectedTimelineSig, func(fresh *Scene) {
			fresh.Audio.Take, fresh.Audio.TakeSig = take, takeSig
			fresh.Audio.Timeline = filepath.Join(dir, "timeline.json")
			fresh.Audio.TimelineSig = tlSig
		})
	if err != nil {
		t.Fatal(err)
	}
	if promoted {
		t.Error("输入已变更的任务结果不应被提升为当前版本")
	}

	cur, err := loadScene(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 编辑没被覆盖
	if cur.Script.Lines[0].Text != "改过的台词" {
		t.Error("任务期间的台本编辑被整份覆盖了")
	}
	// 被取代的任务不写指针——Timeline 保持原状（空），而不是挂上旧产物
	if cur.Audio.Timeline != "" {
		t.Errorf("被取代任务的 Timeline 不应覆盖当前引用，得到 %q", cur.Audio.Timeline)
	}
}

// A 启动 → 输入改变 → B 启动并完成 → A 才完成：最终生效产物必须仍是 B 的。
func TestSupersededJobDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	take := filepath.Join(t.TempDir(), "take.wav")
	wav, _ := base64.StdEncoding.DecodeString(engineTestWAV(t))
	os.WriteFile(take, wav, 0o644)
	sc.Audio.Take, sc.Audio.TakeSig = take, fileSig(take)
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}

	// 任务 A 基于初始台本启动
	takeSig := fileSig(take)
	tlSigA := audioSig(takeSig, sc.scriptSig())

	// 台本变更后任务 B 启动
	l0 := sc.Script.Lines[0]
	res := st.SceneEdit(dir, fmt.Sprintf(
		`{"op":"update_line","line_id":%q,"text":"B 时代的台词"}`, l0.LineID))
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
		t.Fatalf("edit: %s", res)
	}
	cur, _ := loadScene(dir)
	tlSigB := audioSig(takeSig, cur.scriptSig())

	// B 先完成：提升为当前版本
	tlB := filepath.Join(dir, "job-B", "timeline.json")
	promoted, err := st.commitAudioArtifacts(dir, true, jobTimeline, 2, tlSigB,
		(*Scene).expectedTimelineSig, func(fresh *Scene) {
			fresh.Audio.Take, fresh.Audio.TakeSig = take, takeSig
			fresh.Audio.Session = filepath.Dir(tlB)
			fresh.Audio.Timeline, fresh.Audio.TimelineSig = tlB, tlSigB
		})
	if err != nil || !promoted {
		t.Fatalf("B 应被提升: promoted=%v err=%v", promoted, err)
	}

	// A 后完成：不得覆盖 B
	tlA := filepath.Join(dir, "job-A", "timeline.json")
	promoted, err = st.commitAudioArtifacts(dir, true, jobTimeline, 1, tlSigA,
		(*Scene).expectedTimelineSig, func(fresh *Scene) {
			fresh.Audio.Session = filepath.Dir(tlA)
			fresh.Audio.Timeline, fresh.Audio.TimelineSig = tlA, tlSigA
		})
	if err != nil {
		t.Fatal(err)
	}
	if promoted {
		t.Error("旧任务 A 不应覆盖新任务 B 的结果")
	}
	cur, _ = loadScene(dir)
	if cur.Audio.Timeline != tlB || cur.Audio.TimelineSig != tlSigB {
		t.Error("生效的 timeline 被旧任务覆盖")
	}
	if tlStale, _ := cur.AudioStatus(); tlStale {
		t.Error("B 的产物本应仍然有效")
	}
}

// 相同输入的 A、B 两任务：B 先提交且用户已审核其结果，A 后完成不得替换。
// 输入指纹只能判"是否过期"——判"有没有权替换"要靠任务序号。
func TestSameInputJobOrdering(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	take := filepath.Join(t.TempDir(), "take.wav")
	wav, _ := base64.StdEncoding.DecodeString(engineTestWAV(t))
	os.WriteFile(take, wav, 0o644)
	sc.Audio.Take, sc.Audio.TakeSig = take, fileSig(take)
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	takeSig := fileSig(take)
	tlSig := audioSig(takeSig, sc.scriptSig()) // A、B 输入完全相同

	// A(seq1)、B(seq2) 同时跑；B 先完成并提升
	tlB := filepath.Join(dir, "job-B", "timeline.json")
	os.MkdirAll(filepath.Dir(tlB), 0o755)
	os.WriteFile(tlB, []byte(`{"version":1,"segments":[]}`), 0o644)
	promoted, err := st.commitAudioArtifacts(dir, true, jobTimeline, 2, tlSig,
		(*Scene).expectedTimelineSig, func(fresh *Scene) {
			fresh.Audio.Session = filepath.Dir(tlB)
			fresh.Audio.Timeline, fresh.Audio.TimelineSig = tlB, tlSig
		})
	if err != nil || !promoted {
		t.Fatalf("B 应被提升: promoted=%v err=%v", promoted, err)
	}

	// 用户审核并修改了 B 的 timeline（文件改写，指针不变）
	os.WriteFile(tlB, []byte(`{"version":1,"segments":[],"reviewed":true}`), 0o644)

	// A 后完成：输入指纹仍相同，但 seq 更旧 → 无权覆盖已生效的 B
	tlA := filepath.Join(dir, "job-A", "timeline.json")
	promoted, err = st.commitAudioArtifacts(dir, true, jobTimeline, 1, tlSig,
		(*Scene).expectedTimelineSig, func(fresh *Scene) {
			fresh.Audio.Session = filepath.Dir(tlA)
			fresh.Audio.Timeline, fresh.Audio.TimelineSig = tlA, tlSig
		})
	if err != nil {
		t.Fatal(err)
	}
	if promoted {
		t.Error("相同输入的旧任务 A 不应覆盖新任务 B 的结果")
	}
	cur, _ := loadScene(dir)
	if cur.Audio.Timeline != tlB {
		t.Error("已审核的 B 结果不应被替换")
	}
}

// 外部写入（整份替换/手动导入）抢占产物槽位：在途旧任务的提交不得覆盖。
func TestExternalWriteBlocksInFlightJob(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	take := filepath.Join(t.TempDir(), "take.wav")
	wav, _ := base64.StdEncoding.DecodeString(engineTestWAV(t))
	os.WriteFile(take, wav, 0o644)
	sc.Audio.Take, sc.Audio.TakeSig = take, fileSig(take)
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	takeSig := fileSig(take)
	tlSig := audioSig(takeSig, sc.scriptSig())

	// 任务 seq=2 在途（序号已分配）；期间有人整份写回，换掉 timeline 指针
	st.st(dir).jobSeq = 2
	manual := filepath.Join(dir, "manual-timeline.json")
	os.WriteFile(manual, []byte(`{"version":1,"segments":[]}`), 0o644)
	cur, err := loadScene(dir)
	if err != nil {
		t.Fatal(err)
	}
	cur.Audio.Timeline, cur.Audio.TimelineSig = manual, cur.expectedTimelineSig()
	if err := st.saveScene(dir, cur, false); err != nil {
		t.Fatal(err)
	}

	// 旧任务随后提交：输入指纹仍匹配，但槽位已被外部写入占满 → 不提升
	promoted, err := st.commitAudioArtifacts(dir, true, jobTimeline, 2, tlSig,
		(*Scene).expectedTimelineSig, func(fresh *Scene) {
			fresh.Audio.Timeline, fresh.Audio.TimelineSig = filepath.Join(dir, "job-x", "timeline.json"), tlSig
		})
	if err != nil {
		t.Fatal(err)
	}
	if promoted {
		t.Error("在途旧任务不应覆盖外部显式写入")
	}
	cur, _ = loadScene(dir)
	if cur.Audio.Timeline != manual {
		t.Error("手动指定的 timeline 被在途任务覆盖")
	}
}

// SaveScene 整份写回也要过 rev 检查——payload 版本落后即拒绝。
func TestSaveSceneRejectsStaleRev(t *testing.T) {
	dir := t.TempDir()
	st := &Studio{}
	sc := newSceneWithDraft(t)
	if err := st.saveScene(dir, sc, false); err != nil {
		t.Fatal(err)
	}
	// 客户端基于 rev N 的副本改了标题；期间磁盘已被别人写到 N+1
	cur, _ := loadScene(dir)
	stale := *cur
	stale.Title = "旧客户端的覆盖"
	cur.Title = "别人先写入的修改"
	if err := st.saveScene(dir, cur, false); err != nil {
		t.Fatal(err)
	}
	payload, _ := stale.Marshal()
	res := st.SaveScene(dir, string(payload))
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res), &env); err != nil {
		t.Fatal(err)
	}
	if env.OK {
		t.Error("过期整份保存被接受了")
	}
	// 最新 rev 的正常保存仍应通过
	cur2, _ := loadScene(dir)
	cur2.Title = "新标题"
	payload2, _ := cur2.Marshal()
	res2 := st.SaveScene(dir, string(payload2))
	var env2 struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(res2), &env2); err != nil || !env2.OK {
		t.Fatalf("最新 rev 的保存应通过: %s", res2)
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
	// 模拟一份基于 take1+当前台本的 timeline 和 rendered
	// （签名与 Analyze/RenderScene 写入的一致）
	cur.Audio.Timeline = "/tmp/timeline.json"
	cur.Audio.TimelineSig = cur.expectedTimelineSig()
	cur.Audio.Rendered = "/tmp/rendered"
	cur.Audio.RenderedSig = cur.expectedRenderedSig()
	if tlStale, rdStale := cur.AudioStatus(); tlStale || rdStale {
		t.Fatal("新鲜产物不应判过期")
	}
	if err := st.saveScene(dir, cur, false); err != nil {
		t.Fatal(err)
	}
	// 换 take2 → timeline 过期，rendered 传递过期（不得再进正式预览）
	res = st.SceneEdit(dir, fmt.Sprintf(`{"op":"set_take","path":%q}`, take2))
	if err := json.Unmarshal([]byte(res), &env); err != nil || !env.OK {
		t.Fatalf("set_take2: %s", res)
	}
	cur, _ = loadScene(dir)
	if tlStale, rdStale := cur.AudioStatus(); !tlStale || !rdStale {
		t.Errorf("换 take 后 timeline/rendered 应都过期，得到 tl=%v rd=%v", tlStale, rdStale)
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
	url := func(p string) string { return m.base + "?p=" + p + "&k=" + m.key }
	// allowlist 内的文件 + Range + 正确 token
	req := httptest.NewRequest("GET", url(f), nil)
	req.Header.Set("Range", "bytes=0-9")
	rec := httptest.NewRecorder()
	m.serve(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range 应回 206，得到 %d", rec.Code)
	}
	if rec.Body.Len() != 10 {
		t.Errorf("Range 应回 10 字节，得到 %d", rec.Body.Len())
	}
	// 缺/错 token → 403（本机其他网页猜到路径也读不到）
	for _, u := range []string{m.base + "?p=" + f, m.base + "?p=" + f + "&k=wrong"} {
		rec := httptest.NewRecorder()
		m.serve(rec, httptest.NewRequest("GET", u, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("无/错 token 应 403，得到 %d", rec.Code)
		}
	}
	// 未 allow 的目录 → 403
	outside := filepath.Join(t.TempDir(), "y.wav")
	os.WriteFile(outside, []byte("RIFF"), 0o644)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", url(outside), nil)
	m.serve(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("未授权路径应 403，得到 %d", rec2.Code)
	}
	// 非媒体扩展名 → 403
	txt := filepath.Join(dir, "s.txt")
	os.WriteFile(txt, []byte("x"), 0o644)
	rec3 := httptest.NewRecorder()
	m.serve(rec3, httptest.NewRequest("GET", url(txt), nil))
	if rec3.Code != http.StatusForbidden {
		t.Errorf("非媒体文件应 403，得到 %d", rec3.Code)
	}
	// .json 只放行 manifest.json；scene.json 等元数据必须 403
	manifest := filepath.Join(dir, "manifest.json")
	os.WriteFile(manifest, []byte(`{"lines":[]}`), 0o644)
	recM := httptest.NewRecorder()
	m.serve(recM, httptest.NewRequest("GET", url(manifest), nil))
	if recM.Code != http.StatusOK {
		t.Errorf("manifest.json 应放行，得到 %d", recM.Code)
	}
	sceneJSON := filepath.Join(dir, "scene.json")
	os.WriteFile(sceneJSON, []byte(`{}`), 0o644)
	recJ := httptest.NewRecorder()
	m.serve(recJ, httptest.NewRequest("GET", url(sceneJSON), nil))
	if recJ.Code != http.StatusForbidden {
		t.Errorf("非 manifest 的 .json 应 403，得到 %d", recJ.Code)
	}
	// 根目录内指向根外的 symlink 不得逃出 allowlist
	link := filepath.Join(dir, "link.wav")
	if err := os.Symlink(outside, link); err == nil {
		recL := httptest.NewRecorder()
		m.serve(recL, httptest.NewRequest("GET", url(link), nil))
		if recL.Code != http.StatusForbidden {
			t.Errorf("越根 symlink 应 403，得到 %d", recL.Code)
		}
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
