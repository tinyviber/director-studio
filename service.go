// service.go — Studio：绑定给 webview 的工作台服务。
//
// 约定沿用 voice-morph 的 Dubber：所有方法收/回 JSON 字符串
// （{ok: true, data: ...} 或 {ok: false, error: "..."}），前端 bridge.ts
// 统一拆包。UI 线程只做快操作；Analyze/Render 走 goroutine 太重不值——
// 它们本来就是秒级到几十秒的同步任务，前端转菊花即可，进度事件后续再加。
//
// 目录模型（workspace dir 由用户打开/新建，示例见 examples/animatic-demo/workbench/）：
//
//	scenes/<scene_id>/scene.json     — Scene 文档（台本+clips+编排+音频+反馈）
//	audio/rec/                       — 录音 wav（SaveRecording 写这）
//	audio/dubbing/<session>/         — dubbing.Analyze 输出（asr-raw/timeline）
//	exports/                         — 反馈导出（agent 任务 .json/.md）
//
// 可服务媒体根：workspace 目录 + 每个 SetSource 指向的视频所在目录 +
// audio 目录。media.go 的 localhost server 只在这些根下出文件。
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"voice-morph/dubbing"
	"voice-morph/engine"
)

// ---------- JSON 包封 ----------

func okJSON(v any) string {
	b, _ := json.Marshal(map[string]any{"ok": true, "data": v})
	return string(b)
}
func errJSON(err error) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	return string(b)
}
func errf(format string, a ...any) string { return errJSON(fmt.Errorf(format, a...)) }

type Studio struct {
	mu      sync.Mutex
	media   *mediaServer
	scenes  map[string]*sceneState // sceneDir 绝对路径 -> 状态
	sceneMu sync.Mutex
}

type sceneState struct {
	mu   sync.Mutex // 串行化本场景的"读-改-写"（rev 检查与保存必须在同一临界区）
	undo [][]byte
	redo [][]byte
}

func (s *Studio) st(sceneDir string) *sceneState {
	s.sceneMu.Lock()
	defer s.sceneMu.Unlock()
	if s.scenes == nil {
		s.scenes = map[string]*sceneState{}
	}
	key, _ := filepath.Abs(sceneDir)
	if s.scenes[key] == nil {
		s.scenes[key] = &sceneState{}
	}
	return s.scenes[key]
}

func (s *Studio) snapshot(sceneDir string, before *Scene) {
	if before == nil {
		return
	}
	b, err := before.Marshal()
	if err != nil {
		return
	}
	st := s.st(sceneDir)
	st.undo = append(st.undo, b)
	if len(st.undo) > 60 {
		st.undo = st.undo[len(st.undo)-60:]
	}
	st.redo = nil
}

// ---------- 场景文件 ----------

func sceneFile(dir string) string { return filepath.Join(dir, "scene.json") }

func loadScene(dir string) (*Scene, error) {
	b, err := os.ReadFile(sceneFile(dir))
	if err != nil {
		return nil, fmt.Errorf("读 scene.json: %w", err)
	}
	return ParseScene(b)
}

func (s *Studio) saveScene(dir string, sc *Scene, snap bool) error {
	old, _ := loadScene(dir)
	if snap && old != nil {
		s.snapshot(dir, old)
	}
	// Revision 单调递增：以毫秒时间戳为主，undo/外部回退后不回退。
	rev := int(time.Now().UnixMilli())
	if old != nil && rev <= old.Revision {
		rev = old.Revision + 1
	}
	sc.Revision = rev
	if old != nil && sc.NextClipSeq < old.NextClipSeq {
		sc.NextClipSeq = old.NextClipSeq // 分配器单调：undo/回退不回收已发 id
	}
	sc.normalize()
	if err := sc.Validate(); err != nil {
		return err
	}
	b, err := sc.Marshal()
	if err != nil {
		return err
	}
	return dubbing.WriteFileAtomic(sceneFile(dir), b, 0o644)
}

// fileSig 是文件的轻量指纹（大小+mtime）。配音产物有效性靠它判断；
// 音频文件大，不算内容 hash。
func fileSig(p string) string {
	if p == "" {
		return ""
	}
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x:%x", st.Size(), st.ModTime().UnixNano())
}

// decorateScene 在返回前端前注入派生状态：配音链路各产物是否因输入
// 变化而过期（timeline 基于别的 take、rendered 基于别的 timeline）。
func (s *Studio) decorateScene(sc *Scene) {
	if sc.Meta == nil {
		sc.Meta = map[string]any{}
	}
	tlStale, rdStale := sc.AudioStatus()
	sc.Meta["audio_status"] = map[string]any{
		"timeline_stale": tlStale,
		"rendered_stale": rdStale,
	}
}

// ---------- 绑定方法：workspace / scene ----------

func (s *Studio) PickDirectory(title string) string {
	files, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Title: title, Directory: true, CreateDirectories: true})
	if err != nil || len(files) == 0 {
		return errJSON(err)
	}
	return okJSON(map[string]string{"path": files[0]})
}

func (s *Studio) PickFile(title, exts string) string {
	opts := mygo.OpenDialogOptions{Title: title}
	if exts != "" {
		opts.Filters = []mygo.FileFilter{{Name: exts, Extensions: strings.Split(exts, ",")}}
	}
	files, err := mygo.Dialog.Open(opts)
	if err != nil || len(files) == 0 {
		return errJSON(err)
	}
	return okJSON(map[string]string{"path": files[0]})
}

// OpenWorkspace 列出一个工作目录下的 scenes/<id>/。
func (s *Studio) OpenWorkspace(dir string) string {
	if dir == "" {
		return errf("workspace 目录为空")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "scenes"))
	out := []map[string]any{}
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sd := filepath.Join(dir, "scenes", e.Name())
			if sc, err := loadScene(sd); err == nil {
				out = append(out, map[string]any{
					"scene_id": sc.SceneID, "title": sc.Title,
					"dir": sd, "clips": len(sc.Clips), "lines": len(sc.Script.Lines),
				})
			}
		}
	}
	s.mediaSrv().allow(dir)
	return okJSON(map[string]any{"dir": dir, "scenes": out})
}

// NewScene 在 workspace 下新建 scene 目录和骨架 scene.json。
func (s *Studio) NewScene(workspace, title string) string {
	if title == "" {
		title = "未命名场景"
	}
	slug := slugify(title)
	if slug == "" {
		slug = fmt.Sprintf("s%d", time.Now().Unix()%10000)
	}
	dir := filepath.Join(workspace, "scenes", slug)
	for i := 2; ; i++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dir = filepath.Join(workspace, "scenes", fmt.Sprintf("%s-%d", slug, i))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errJSON(err)
	}
	sc := &Scene{
		Version: SceneVersion, SceneID: filepath.Base(dir), Title: title,
		Roles: []Role{
			{ID: "a", Name: "角色甲", Color: "#4a90d9", Voice: &dubbing.VoiceProfile{Backend: dubbing.VoiceBackendDSP, Preset: "normal"}},
			{ID: "b", Name: "角色乙", Color: "#d9634a", Voice: &dubbing.VoiceProfile{Backend: dubbing.VoiceBackendDSP, Preset: "bright"}},
		},
		Script: dubbing.Script{Version: 1},
	}
	if err := s.saveScene(dir, sc, false); err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]any{"dir": dir, "scene": sc})
}

func (s *Studio) LoadScene(dir string) string {
	sc, err := loadScene(dir)
	if err != nil {
		return errJSON(err)
	}
	sc.normalize()
	s.mediaSrv().allow(dir)
	s.decorateScene(sc)
	return okJSON(sc)
}

// SaveScene 全量写回（编辑操作走 SceneEdit 更细粒度且带 undo）。
// payload 里的 rev 即客户端的基准版本：落后于磁盘当前版本说明期间
// 被别人改过，拒绝整份覆盖（rev==0 视为无版本概念的导入，跳过检查）。
func (s *Studio) SaveScene(dir, payload string) string {
	st := s.st(dir)
	st.mu.Lock()
	defer st.mu.Unlock()
	sc, err := ParseScene([]byte(payload))
	if err != nil {
		return errJSON(err)
	}
	if old, _ := loadScene(dir); old != nil && sc.Revision != 0 && sc.Revision < old.Revision {
		return errf("scene 已被修改（rev %d > %d），请刷新后重试", old.Revision, sc.Revision)
	}
	if err := s.saveScene(dir, sc, true); err != nil {
		return errJSON(err)
	}
	s.decorateScene(sc)
	return okJSON(sc)
}

// GenDraft 从台本重生成 draft clips；已有 final 素材绑定随 line_ids 迁移。
func (s *Studio) GenDraft(dir string) string {
	t0 := time.Now()
	st := s.st(dir)
	st.mu.Lock()
	defer st.mu.Unlock()
	sc, err := loadScene(dir)
	if err != nil {
		return errJSON(err)
	}
	before, _ := loadScene(dir)
	clips, warns, err := GenDraft(sc, sc.Clips)
	if err != nil {
		return errJSON(err)
	}
	sc.Clips = clips
	sc.Order = nil
	for _, c := range clips {
		sc.Order = append(sc.Order, c.ClipID)
	}
	if err := s.saveScene(dir, sc, false); err != nil {
		return errJSON(err)
	}
	s.snapshot(dir, before)
	s.decorateScene(sc)
	return okJSON(map[string]any{"scene": sc, "warnings": warns,
		"elapsed_ms": float64(time.Since(t0)) / 1e6})
}

// SceneEdit 应用一个编辑操作：{"op":"reorder"|"split"|"delete"|"duplicate"|
// "trim"|"set_source"|"update_line"|"add_line"|"delete_line"|"add_feedback"|
// "feedback_status"|"set_role_voice"|"note", ...}
func (s *Studio) SceneEdit(dir, opJSON string) (ret string) {
	// 前端 agent 也可能直接调这个绑定——畸形请求不能打崩 UI 进程。
	defer func() {
		if r := recover(); r != nil {
			ret = errf("scene_edit: 非法请求 %v", r)
		}
	}()
	var op map[string]any
	if err := json.Unmarshal([]byte(opJSON), &op); err != nil {
		return errJSON(fmt.Errorf("op JSON: %w", err))
	}
	st := s.st(dir)
	st.mu.Lock()
	defer st.mu.Unlock()
	sc, err := loadScene(dir)
	if err != nil {
		return errJSON(err)
	}
	// 乐观并发：调用方声明自己基于哪个修订版；不匹配说明场景在两次
	// 读取之间被改过，拒绝这次写，避免静默覆盖别人的变更。
	if br, ok := op["base_rev"].(float64); ok && int(br) != sc.Revision {
		return errf("scene 已被修改（rev %d ≠ %d），请刷新后重试", sc.Revision, int(br))
	}
	kind, _ := op["op"].(string)
	str := func(k string) string { v, _ := op[k].(string); return v }
	num := func(k string) int { v, _ := op[k].(float64); return int(v) }

	var opErr error
	switch kind {
	case "reorder":
		raw, ok := op["order"].([]any)
		if !ok {
			return errf("reorder: order 必须是数组")
		}
		var order []string
		for _, v := range raw {
			sv, ok := v.(string)
			if !ok {
				return errf("reorder: order 元素必须是字符串")
			}
			order = append(order, sv)
		}
		opErr = sc.ReorderClips(order)
	case "split":
		opErr = sc.SplitClip(str("clip_id"), num("at_ms"))
	case "delete":
		opErr = sc.DeleteClip(str("clip_id"))
	case "duplicate":
		opErr = sc.DuplicateClip(str("clip_id"))
	case "trim":
		opErr = sc.TrimClip(str("clip_id"), num("dur_ms"))
	case "set_source":
		opErr = sc.SetSource(str("clip_id"), str("kind"), str("path"), num("t0_ms"), num("t1_ms"))
		if opErr == nil && str("path") != "" {
			s.mediaSrv().allow(filepath.Dir(str("path")))
		}
	case "update_line":
		opErr = sc.UpdateLine(str("line_id"), str("text"), str("role_id"), str("direction"))
	case "add_line":
		var id string
		id, opErr = sc.AddLine(str("after_id"), str("role_id"), str("text"), str("direction"))
		if opErr == nil {
			op["_new_id"] = id
		}
	case "delete_line":
		opErr = sc.DeleteLine(str("line_id"))
	case "add_feedback":
		fb := Feedback{
			SceneMs: num("scene_ms"), ClipID: str("clip_id"), ClipMs: num("clip_ms"),
			LineID: str("line_id"), TakeRef: str("take_ref"), Kind: str("kind"),
			Text: str("text"), Voice: str("voice"),
			Created: time.Now().Format(time.RFC3339),
		}
		var id string
		id, opErr = sc.AddFeedback(fb)
		if opErr == nil {
			op["_new_id"] = id
		}
	case "feedback_status":
		opErr = sc.SetFeedbackStatus(str("id"), str("status"), str("resolution"))
	case "set_role_voice":
		r := sc.role(str("role_id"))
		if r == nil {
			opErr = fmt.Errorf("无角色 %q", str("role_id"))
			break
		}
		r.Voice = &dubbing.VoiceProfile{Backend: dubbing.VoiceBackendDSP, Preset: str("preset")}
		if str("preset") == "" {
			var p dubbing.VoiceProfile
			if err := json.Unmarshal([]byte(str("profile")), &p); err == nil && p.Backend != "" {
				r.Voice = &p
			}
		}
	case "note":
		c := sc.clip(str("clip_id"))
		if c == nil {
			opErr = fmt.Errorf("无 clip %q", str("clip_id"))
		} else {
			c.Note = str("text")
		}
	case "set_take":
		p := str("path")
		if p == "" {
			opErr = fmt.Errorf("set_take: 空路径")
			break
		}
		if _, err := os.Stat(p); err != nil {
			opErr = fmt.Errorf("set_take: %w", err)
			break
		}
		// 只换指纹不删下游产物——timeline/rendered 的 sig 从此对不上，
		// UI 显示"已过期，需重新处理"，由导演决定何时重跑。
		sc.Audio.Take = p
		sc.Audio.TakeSig = fileSig(p)
		s.mediaSrv().allow(filepath.Dir(p))
	default:
		return errf("未知 op %q", kind)
	}
	if opErr != nil {
		return errJSON(opErr)
	}
	if err := s.saveScene(dir, sc, true); err != nil {
		return errJSON(err)
	}
	s.decorateScene(sc)
	return okJSON(map[string]any{"scene": sc, "applied": op})
}

func (s *Studio) Undo(dir string) string {
	st := s.st(dir)
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.undo) == 0 {
		return errf("没有可撤销的操作")
	}
	cur, err := loadScene(dir)
	if err != nil {
		return errJSON(err)
	}
	b := st.undo[len(st.undo)-1]
	st.undo = st.undo[:len(st.undo)-1]
	if cb, err := cur.Marshal(); err == nil {
		st.redo = append(st.redo, cb)
	}
	prev, err := ParseScene(b)
	if err != nil {
		return errJSON(err)
	}
	if err := s.saveScene(dir, prev, false); err != nil {
		return errJSON(err)
	}
	s.decorateScene(prev)
	return okJSON(prev)
}

func (s *Studio) Redo(dir string) string {
	st := s.st(dir)
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.redo) == 0 {
		return errf("没有可重做的操作")
	}
	b := st.redo[len(st.redo)-1]
	st.redo = st.redo[:len(st.redo)-1]
	cur, _ := loadScene(dir)
	if cb, err := cur.Marshal(); err == nil {
		st.undo = append(st.undo, cb)
	}
	next, err := ParseScene(b)
	if err != nil {
		return errJSON(err)
	}
	if err := s.saveScene(dir, next, false); err != nil {
		return errJSON(err)
	}
	s.decorateScene(next)
	return okJSON(next)
}

// ---------- 配音链路（复用 voice-morph/dubbing + engine）----------

// SaveRecording 把前端录的 wav（base64）落到 <workspace>/audio/rec/。
func (s *Studio) SaveRecording(workspace, b64wav, name string) string {
	raw, err := base64.StdEncoding.DecodeString(b64wav)
	if err != nil {
		return errJSON(fmt.Errorf("base64: %w", err))
	}
	if len(raw) < 44 || string(raw[:4]) != "RIFF" {
		return errf("不是 WAV 数据")
	}
	if name == "" {
		name = fmt.Sprintf("take-%s", time.Now().Format("150405"))
	}
	name = strings.TrimSuffix(filepath.Base(name), ".wav")
	dir := filepath.Join(workspace, "audio", "rec")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errJSON(err)
	}
	// 文件名只有秒级精度时连录两条会撞名——存在即追加序号，不覆盖旧 take。
	p := filepath.Join(dir, name+".wav")
	for i := 2; ; i++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			break
		}
		p = filepath.Join(dir, fmt.Sprintf("%s-%d.wav", name, i))
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return errJSON(err)
	}
	s.mediaSrv().allow(dir)
	return okJSON(map[string]string{"path": p})
}

// Analyze 对一条 take 跑 voice-morph 的 ASR + 台本对齐，输出会话目录。
// takePath 相对 workspace 或绝对路径；script 来自 scene.json 现场转存。
func (s *Studio) Analyze(sceneDir, takePath string) string {
	sc, err := loadScene(sceneDir)
	if err != nil {
		return errJSON(err)
	}
	if !filepath.IsAbs(takePath) {
		takePath = filepath.Join(filepath.Dir(filepath.Dir(sceneDir)), takePath)
	}
	if _, err := os.Stat(takePath); err != nil {
		return errJSON(fmt.Errorf("take: %w", err))
	}
	outBase := filepath.Join(filepath.Dir(filepath.Dir(sceneDir)), "audio", "dubbing")
	if err := os.MkdirAll(outBase, 0o755); err != nil {
		return errJSON(err)
	}
	// 把 scene 内嵌台本落成 dubbing.Script 文件供 worker/reconcile 用
	tmpScript := filepath.Join(outBase, "script-"+sc.SceneID+".json")
	sb, _ := json.MarshalIndent(sc.Script, "", "  ")
	if err := dubbing.WriteFileAtomic(tmpScript, sb, 0o644); err != nil {
		return errJSON(err)
	}
	t0 := time.Now()
	asr, tl, err := dubbing.Analyze(dubbing.AnalyzeOptions{
		AudioPath: takePath, ScriptPath: tmpScript, OutDir: outBase,
	})
	analyzeMs := float64(time.Since(t0)) / 1e6
	if err != nil {
		return errJSON(err)
	}
	// ASR 耗时期间可能有编辑进来——把产物合并到最新 scene 上，
	// 不能拿任务开始时的旧文档整份覆盖。
	st := s.st(sceneDir)
	st.mu.Lock()
	defer st.mu.Unlock()
	fresh, err := loadScene(sceneDir)
	if err != nil {
		return errJSON(err)
	}
	fresh.Audio.Take = takePath
	fresh.Audio.TakeSig = fileSig(takePath)
	fresh.Audio.Session = filepath.Dir(tl.ASRFile)
	if fresh.Audio.Session == "." {
		fresh.Audio.Session = outBase
	}
	fresh.Audio.Timeline = filepath.Join(fresh.Audio.Session, "timeline.json")
	fresh.Audio.TimelineSig = fresh.expectedTimelineSig() // take+台本指纹
	if err := s.saveScene(sceneDir, fresh, true); err != nil {
		return errJSON(err)
	}
	s.mediaSrv().allow(fresh.Audio.Session)
	return okJSON(map[string]any{
		"session": fresh.Audio.Session, "timeline": fresh.Audio.Timeline,
		"segments": len(tl.Segments), "review_count": tl.ReviewCount,
		"unmatched": len(tl.UnmatchedLines), "asr": asr != nil,
		"elapsed_ms": analyzeMs,
	})
}

// RenderScene 跑 dubbing.Render：timeline + voices.json → rendered lines + manifest。
func (s *Studio) RenderScene(sceneDir string) string {
	sc, err := loadScene(sceneDir)
	if err != nil {
		return errJSON(err)
	}
	if sc.Audio.Timeline == "" {
		return errf("还没有 timeline——先 Analyze 或手动指定")
	}
	ws := filepath.Dir(filepath.Dir(sceneDir))
	vp := filepath.Join(ws, "audio", "voices-"+sc.SceneID+".json")
	v := dubbing.Voices{Version: 1, Voices: map[string]dubbing.VoiceProfile{}}
	for _, r := range sc.Roles {
		if r.Voice != nil {
			v.Voices[r.ID] = *r.Voice
		}
	}
	vb, _ := json.MarshalIndent(v, "", "  ")
	if err := dubbing.WriteFileAtomic(vp, vb, 0o644); err != nil {
		return errJSON(err)
	}
	outDir := filepath.Join(sc.Audio.Session, "rendered")
	t0 := time.Now()
	m, err := dubbing.Render(dubbing.RenderOptions{
		TimelinePath: sc.Audio.Timeline, VoicesPath: vp, OutDir: outDir,
		WriteSrc: true, AllowUnreviewed: false,
	})
	if err != nil {
		return errJSON(err)
	}
	// 渲染耗时期间可能改过音色/timeline——合并到最新文档上。
	st := s.st(sceneDir)
	st.mu.Lock()
	defer st.mu.Unlock()
	fresh, err := loadScene(sceneDir)
	if err != nil {
		return errJSON(err)
	}
	fresh.Audio.Rendered = outDir
	fresh.Audio.RenderedSig = fresh.expectedRenderedSig() // timeline+音色指纹
	if err := s.saveScene(sceneDir, fresh, false); err != nil {
		return errJSON(err)
	}
	s.mediaSrv().allow(outDir)
	return okJSON(map[string]any{
		"rendered": outDir, "status": m.Status, "lines": len(m.Lines),
		"warnings": m.Warnings, "skipped": len(m.Skipped),
		"elapsed_ms": float64(time.Since(t0)) / 1e6,
	})
}

// LoadTimeline / SaveTimeline / MergeTimeline — 直接套 dubbing 的读写。
func (s *Studio) LoadTimeline(path string) string {
	tl, err := dubbing.LoadTimeline(path)
	if err != nil {
		return errJSON(err)
	}
	return okJSON(tl)
}
func (s *Studio) SaveTimeline(path, payload string) string {
	tl, err := dubbing.ParseTimeline([]byte(payload))
	if err != nil {
		return errJSON(err)
	}
	tl.RefreshReviewCount()
	return okJSON(tl) // 不直接写盘——SaveTimelineFile 才算数
}
func (s *Studio) SaveTimelineFile(path, payload string) string {
	tl, err := dubbing.ParseTimeline([]byte(payload))
	if err != nil {
		return errJSON(err)
	}
	tl.RefreshReviewCount()
	b, err := json.MarshalIndent(tl, "", "  ")
	if err != nil {
		return errJSON(err)
	}
	if err := dubbing.WriteFileAtomic(path, b, 0o644); err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]any{"path": path})
}
func (s *Studio) MergeReviewed(draftPath, reviewedPath string) string {
	d, err := dubbing.LoadTimeline(draftPath)
	if err != nil {
		return errJSON(err)
	}
	r, err := dubbing.LoadTimeline(reviewedPath)
	if err != nil {
		return errJSON(err)
	}
	m, err := dubbing.MergeReviewed(d, r)
	if err != nil {
		return errJSON(err)
	}
	return okJSON(m)
}

// ---------- 变声试听 / 模型 ----------

// MorphFile 用 engine 离线处理一个 wav（返回 base64 wav + 耗时）。
func (s *Studio) MorphFile(path, presetOrParamsJSON string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errJSON(err)
	}
	pcm, sr, err := engine.DecodeWAV(raw)
	if err != nil {
		return errJSON(err)
	}
	eng := engine.New()
	var p engine.Params
	if err := json.Unmarshal([]byte(presetOrParamsJSON), &p); err == nil && (p.Strength != 0 || p.Pitch != 0 || p.Timbre != 0 || p.FormantShape != 0 || p.TargetF0 != 0) {
		if p.Strength == 0 {
			p.Strength = 1
		}
		if p.Gain == 0 {
			p.Gain = 1
		}
		if p.PitchRange == 0 {
			p.PitchRange = 1
		}
		eng.SetParams(p)
	} else {
		for _, pr := range engine.Presets {
			if pr.ID == presetOrParamsJSON {
				eng.SetParams(pr.Params)
				break
			}
		}
	}
	t0 := time.Now()
	out := eng.Render(pcm)
	return okJSON(map[string]any{
		"wav":        base64.StdEncoding.EncodeToString(engine.EncodeWAV(out, engine.SampleRate)),
		"elapsed_ms": float64(time.Since(t0)) / 1e6, "in_hz": sr,
	})
}

func (s *Studio) PresetList() string { return okJSON(engine.Presets) }

func (s *Studio) ModelStatus() string {
	cfg, err := dubbing.LoadUserConfig("")
	if err != nil {
		return errJSON(err)
	}
	ms, err := dubbing.ResolveModel(cfg, "")
	if err != nil {
		return errJSON(err)
	}
	return okJSON(ms)
}
func (s *Studio) ScanModels() string {
	cfg, err := dubbing.LoadUserConfig("")
	if err != nil {
		return errJSON(err)
	}
	return okJSON(dubbing.ScanModels(cfg))
}
func (s *Studio) SetModel(dir, vadDir string) string {
	cfg, err := dubbing.LoadUserConfig("")
	if err != nil {
		return errJSON(err)
	}
	nd, err := dubbing.NormalizePath(dir)
	if err != nil {
		return errJSON(err)
	}
	c := dubbing.ValidateModelDir(nd, "sensevoice")
	if !c.Valid {
		return errf("模型目录校验失败：%s", c.Detail)
	}
	cfg.ModelDir = nd
	if vadDir != "" {
		if nv, err := dubbing.NormalizePath(vadDir); err == nil {
			cfg.VADDir = nv
		}
	}
	if err := cfg.Save(""); err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]string{"model_dir": nd, "vad_dir": cfg.VADDir})
}

// ---------- 反馈导出 ----------

// ExportFeedback 把 scene 的反馈导出为 exports/fb-<ts>.{json,md}（agent 任务格式）。
func (s *Studio) ExportFeedback(sceneDir string) string {
	sc, err := loadScene(sceneDir)
	if err != nil {
		return errJSON(err)
	}
	open := []Feedback{}
	for _, f := range sc.Feedback {
		if f.Status == "open" || f.Status == "doing" {
			open = append(open, f)
		}
	}
	if len(open) == 0 {
		return errf("没有 open/doing 状态的反馈")
	}
	ws := filepath.Dir(filepath.Dir(sceneDir))
	expDir := filepath.Join(ws, "exports")
	if err := os.MkdirAll(expDir, 0o755); err != nil {
		return errJSON(err)
	}
	ts := time.Now().Format("20060102-150405")
	jp := filepath.Join(expDir, fmt.Sprintf("fb-%s-%s.json", sc.SceneID, ts))
	mp := filepath.Join(expDir, fmt.Sprintf("fb-%s-%s.md", sc.SceneID, ts))

	type task struct {
		Feedback
		SceneTitle string `json:"scene_title"`
		ClipTitle  string `json:"clip_title,omitempty"`
		LineText   string `json:"line_text,omitempty"`
		Context    string `json:"context"` // 相邻 clips
	}
	tasks := []task{}
	var md strings.Builder
	md.WriteString(fmt.Sprintf("# 导演反馈导出 — %s (%s)\n\n", sc.Title, sc.SceneID))
	for _, f := range open {
		t := task{Feedback: f, SceneTitle: sc.Title}
		if c := sc.clip(f.ClipID); c != nil {
			t.ClipTitle = c.Title
			// 上下文：前后各一个 clip
			ids := sc.Order
			i := indexOfStr(ids, f.ClipID)
			ctx := []string{}
			if i > 0 {
				ctx = append(ctx, ids[i-1])
			}
			ctx = append(ctx, f.ClipID)
			if i >= 0 && i < len(ids)-1 {
				ctx = append(ctx, ids[i+1])
			}
			t.Context = strings.Join(ctx, " → ")
		}
		if l := sc.line(f.LineID); l != nil {
			t.LineText = l.Text
		}
		tasks = append(tasks, t)
		md.WriteString(fmt.Sprintf("## %s [%s] @ %.1fs\n\n", f.ID, f.Kind, float64(f.SceneMs)/1000))
		if f.ClipID != "" {
			md.WriteString(fmt.Sprintf("- 目标：Scene %s / Clip %s（%s），clip 内 %.1fs\n", sc.SceneID, f.ClipID, t.ClipTitle, float64(f.ClipMs)/1000))
		}
		if t.LineText != "" {
			md.WriteString(fmt.Sprintf("- 相关台词：%s —「%s」\n", f.LineID, t.LineText))
		}
		if t.Context != "" {
			md.WriteString(fmt.Sprintf("- 上下文：%s\n", t.Context))
		}
		md.WriteString(fmt.Sprintf("- 问题：%s\n", f.Text))
		md.WriteString("- 限制：不要重做未受影响的 Clips；改动需落在同一 scene_id/clip_id 体系内。\n\n")
	}
	jb, _ := json.MarshalIndent(map[string]any{
		"version": 1, "scene_id": sc.SceneID, "exported": ts,
		"constraints": "keep scene_id/clip_id/line_id stable; do not regenerate untouched clips",
		"tasks":       tasks,
	}, "", "  ")
	if err := dubbing.WriteFileAtomic(jp, jb, 0o644); err != nil {
		return errJSON(err)
	}
	if err := dubbing.WriteFileAtomic(mp, []byte(md.String()), 0o644); err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]string{"json": jp, "md": mp})
}

// ---------- 杂项文件 IO ----------

func (s *Studio) ReadFileB64(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]any{"b64": base64.StdEncoding.EncodeToString(b), "bytes": len(b)})
}
func (s *Studio) LoadTextFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]any{"text": string(b)})
}
func (s *Studio) SaveTextFile(path, payload string) string {
	if err := dubbing.WriteFileAtomic(path, []byte(payload), 0o644); err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]string{"path": path})
}
func (s *Studio) OpenPath(path string) string {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return errJSON(err)
	}
	return okJSON(true)
}
func (s *Studio) Confirm(title, content string) string {
	res, err := mygo.Dialog.Message(mygo.MessageOptions{
		Type: mygo.MessageQuestion, Title: title, Message: content,
		Buttons: []string{"取消", "确定"}, DefaultButton: 0, CancelButton: 0})
	if err != nil {
		return errJSON(err)
	}
	return okJSON(map[string]bool{"ok": res.Button == 1})
}
func (s *Studio) Message(title, content string) {
	mygo.Dialog.Message(mygo.MessageOptions{
		Type: mygo.MessageInfo, Title: title, Message: content})
}

// ScanVideos 列目录（递归一层）里的视频文件，用于给 clip 绑定正式素材。
func (s *Studio) ScanVideos(dir string) string {
	out := []string{}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if p != dir && d != nil && d.IsDir() && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext == ".mp4" || ext == ".webm" || ext == ".mov" {
			out = append(out, p)
		}
		if len(out) > 500 {
			return fmt.Errorf("too many")
		}
		return nil
	})
	if len(out) > 0 {
		s.mediaSrv().allow(dir)
	}
	return okJSON(out)
}

// MediaBase 返回 localhost 媒体服务基址和会话令牌
// （前端拼 ?p=<abs path>&k=<key>）。
func (s *Studio) MediaBase() string {
	m := s.mediaSrv()
	return okJSON(map[string]string{"base": m.base, "key": m.key})
}

func (s *Studio) mediaSrv() *mediaServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.media == nil {
		m, err := newMediaServer()
		if err != nil {
			log.Printf("media server: %v", err)
			s.media = &mediaServer{} // 兜底：无服务时 MediaBase 返空串，前端走 b64
			return s.media
		}
		s.media = m
	}
	return s.media
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '/' || r == '\\' || r == ':' {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(strings.ReplaceAll(b.String(), " ", "-"))
	if len([]rune(out)) > 24 {
		out = string([]rune(out)[:24])
	}
	return out
}
