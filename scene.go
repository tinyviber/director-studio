// Package main — Director Studio 的数据模型与场景编辑操作。
//
// 一个 scene 目录一份 scene.json，是台本、draft clips、编排顺序、音频状态
// 和导演反馈的单一数据源（身份与验收走各自显式操作，不被编辑操作静默改写）：
//
//	scene.json: {version, scene_id, title, roles, script(dubbing.Script),
//	             clips[], order[], audio{}, feedback[]}
//
// 关键约定：
//   - 台本行用稳定 line_id 关联，不按行号；clips 记 line_ids 依赖。
//   - clip.source.kind = draft | video；draft 由 draftspec 实时画在 canvas 上，
//     video 播真实素材文件。candidates[] 保留可替换版本，pick 选当前用哪个。
//   - 改台词只把包含该 line 的 clip 标 stale，不触发重生成；重排只改 order。
//   - feedback 绑定 (scene_ms, clip_id, clip_ms, line_id, take_ref)，
//     status: open|doing|done|wontfix，可导出为 agent 任务文件。
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"voice-morph/dubbing"
)

const SceneVersion = 1

// ---------- 模型 ----------

type Role struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
	// Voice 直接复用 dubbing.VoiceProfile（backend:"dsp" + preset 或完整 params）。
	Voice *dubbing.VoiceProfile `json:"voice,omitempty"`
}

// BeatEvent 是 clip 内的一个时间化事件：台词、动作、镜头、音效或标注。
type BeatEvent struct {
	AtMs   int       `json:"at_ms"`
	Kind   string    `json:"kind"` // line|action|camera|sfx|note
	LineID string    `json:"line_id,omitempty"`
	Actor  string    `json:"actor,omitempty"`  // role id
	Action string    `json:"action,omitempty"` // stand|sit|walk|point|talk|turn|surprise|smile|sad|nod|shake|enter|exit|idle
	DurMs  int       `json:"dur_ms,omitempty"`
	From   []float64 `json:"from,omitempty"` // [x,y] 0..1 归一化
	To     []float64 `json:"to,omitempty"`
	Text   string    `json:"text,omitempty"` // camera 镜头名 / sfx 名 / note 文本
}

// ClipSource 描述 clip 的视觉来源；kind=draft（canvas 预演）或 video（正式素材）。
type ClipSource struct {
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
}

type Clip struct {
	ClipID string      `json:"clip_id"`
	Title  string      `json:"title,omitempty"`
	DurMs  int         `json:"dur_ms"`
	Beats  []BeatEvent `json:"beats"`
	// LineIDs 是本 clip 依赖的台本行（顺序）。台词改动 → 只标这些 clip stale。
	LineIDs []string `json:"line_ids,omitempty"`
	// Candidates 是可替换版本（draft 占位永远在 candidates[0]）；Pick 指当前用哪个。
	Candidates []ClipSource `json:"candidates,omitempty"`
	Pick       int          `json:"pick"`
	Status     string       `json:"status"` // draft|final|approved
	Stale      bool         `json:"stale,omitempty"`
	T0Ms       int          `json:"t0_ms,omitempty"` // 视频素材入点（ms）
	T1Ms       int          `json:"t1_ms,omitempty"` // 视频素材出点（0=到尾）
	Note       string       `json:"note,omitempty"`
	Risk       string       `json:"risk,omitempty"` // 无法实现 draft 意图时的技术风险说明
}

func (c *Clip) Source() ClipSource {
	if len(c.Candidates) == 0 {
		return ClipSource{Kind: "draft"}
	}
	p := c.Pick
	if p < 0 || p >= len(c.Candidates) {
		p = 0
	}
	return c.Candidates[p]
}

func (c *Clip) IsFinal() bool { return c.Source().Kind == "video" }

// Feedback 是一条导演批注（可含语音反馈的转写引用）。
type Feedback struct {
	ID         string `json:"id"`
	Created    string `json:"created"`
	SceneMs    int    `json:"scene_ms"`
	ClipID     string `json:"clip_id,omitempty"`
	ClipMs     int    `json:"clip_ms,omitempty"`
	LineID     string `json:"line_id,omitempty"`
	TakeRef    string `json:"take_ref,omitempty"` // 关联音频段/文件
	Kind       string `json:"kind"`               // logic|pacing|visual|audio|other
	Text       string `json:"text"`
	Voice      string `json:"voice,omitempty"`      // 语音反馈的 wav 路径
	VoiceText  string `json:"voice_text,omitempty"` // ASR 转写（若跑过）
	Status     string `json:"status"`               // open|doing|done|wontfix
	Resolution string `json:"resolution,omitempty"` // 处理说明/所用版本
}

// AudioState 记录这个 scene 的配音链路产物位置。
// 产物关系是 Script→Take→Timeline→Rendered 的依赖链：每条产物记下它
// 基于的输入指纹（sig），上游一变下游自动判过期——不删文件，但工作台
// 能明确显示"输入已变化，需要重新处理"，而不是继续放不确定的音频。
type AudioState struct {
	Take        string `json:"take,omitempty"`         // 当前录音（audio/rec/*.wav）
	TakeSig     string `json:"take_sig,omitempty"`     // 设置 take 时的文件指纹
	Session     string `json:"session,omitempty"`      // dubbing 会话目录
	Timeline    string `json:"timeline,omitempty"`     // 生效的 timeline(-reviewed).json
	TimelineSig string `json:"timeline_sig,omitempty"` // timeline 基于的 take 指纹
	Rendered    string `json:"rendered,omitempty"`     // rendered/ 目录（manifest.json 在内）
	RenderedSig string `json:"rendered_sig,omitempty"` // rendered 基于的 timeline 指纹
	// RenderedStatus 持久化 dubbing.Render 的结果状态（complete|partial|
	// blocked_review|failed）。指纹一致只代表"输入没过期"，不代表产物
	// 完整——部分台词缺失的产物不能被当成完整版本进正式预览。
	RenderedStatus string `json:"rendered_status,omitempty"`
}

// renderedUsable：rendered 产物能否进正式预览。complete 或没有状态
// 记录的旧数据可用；partial/blocked_review/failed 说明有台词缺失
// 或渲染失败，只能算中间产物，由配音面板提示后重渲。
func (a *AudioState) renderedUsable() bool {
	return a.Rendered != "" && (a.RenderedStatus == "" || a.RenderedStatus == "complete")
}

// audioSig 把多个输入指纹合成一个短指纹（顺序敏感）。
func audioSig(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// scriptSig 台本指纹：只含影响配音的字段（line_id/role_id/text）——
// direction 改动不影响音频，不算进指纹。
func (s *Scene) scriptSig() string {
	type l struct {
		ID, Role, Text string
	}
	lines := make([]l, 0, len(s.Script.Lines))
	for _, x := range s.Script.Lines {
		lines = append(lines, l{x.LineID, x.RoleID, x.Text})
	}
	b, _ := json.Marshal(lines)
	return audioSig(string(b))
}

// voicesSig 角色变声参数指纹：预设或自定义参数变了，rendered 就该重渲。
// 只含 id+voice——角色改名/换色不影响音频，不算进指纹。
func (s *Scene) voicesSig() string {
	type rv struct {
		ID    string                `json:"id"`
		Voice *dubbing.VoiceProfile `json:"voice"`
	}
	rs := make([]rv, 0, len(s.Roles))
	for _, r := range s.Roles {
		rs = append(rs, rv{r.ID, r.Voice})
	}
	b, _ := json.Marshal(rs)
	return audioSig(string(b))
}

// expectedTimelineSig：当前 take + 台本应产生的 timeline 指纹。
func (s *Scene) expectedTimelineSig() string {
	return audioSig(s.Audio.TakeSig, s.scriptSig())
}

// expectedRenderedSig：当前 timeline + 变声参数应产生的 rendered 指纹。
func (s *Scene) expectedRenderedSig() string {
	return audioSig(fileSig(s.Audio.Timeline), s.voicesSig())
}

// AudioStatus 判断配音产物是否仍有效。过期是传递性的：
// take/台本变了 → timeline 和 rendered 都过期；timeline/变声变了 → rendered 过期。
func (s *Scene) AudioStatus() (timelineStale, renderedStale bool) {
	a := s.Audio
	if a.Timeline != "" && a.TimelineSig != s.expectedTimelineSig() {
		timelineStale = true
	}
	if a.Rendered != "" && (timelineStale || a.RenderedSig != s.expectedRenderedSig()) {
		renderedStale = true
	}
	return
}

// Scene 是一个连续叙事/审核单位：台本 + draft clips + 编排 + 音频 + 反馈。
type Scene struct {
	Version int `json:"version"`
	// Revision 每次写盘单调递增；SceneEdit 带 base_rev 做乐观并发检查，
	// 防止前端基于过期副本提交、覆盖掉别人（含 agent）刚写入的改动。
	Revision int `json:"rev"`
	// NextClipSeq 是 clip_id 的单调分配器：分配过的序号永不复用——
	// 否则被淘汰的 clip_id 会在后续重生成中指到不同内容，让旧批注
	// 静默挂错对象。它随 scene.json 持久化，跨任意多次重生成有效。
	NextClipSeq int            `json:"next_clip_seq,omitempty"`
	SceneID     string         `json:"scene_id"`
	Title       string         `json:"title"`
	Roles       []Role         `json:"roles"`
	Script      dubbing.Script `json:"script"`
	Clips       []Clip         `json:"clips"`
	Order       []string       `json:"order"`
	Audio       AudioState     `json:"audio,omitempty"`
	Feedback    []Feedback     `json:"feedback,omitempty"`
	Meta        map[string]any `json:"meta,omitempty"`
}

func (s *Scene) clip(id string) *Clip {
	for i := range s.Clips {
		if s.Clips[i].ClipID == id {
			return &s.Clips[i]
		}
	}
	return nil
}

func (s *Scene) line(id string) *dubbing.ScriptLine { return s.Script.Line(id) }

func (s *Scene) role(id string) *Role {
	for i := range s.Roles {
		if s.Roles[i].ID == id {
			return &s.Roles[i]
		}
	}
	return nil
}

// Validate 检查结构不变量；写盘前必须过。
func (s *Scene) Validate() error {
	if s.Version < 1 || s.Version > SceneVersion {
		return fmt.Errorf("scene: unsupported version %d", s.Version)
	}
	if s.SceneID == "" {
		return fmt.Errorf("scene: empty scene_id")
	}
	ids := map[string]bool{}
	for i, c := range s.Clips {
		if c.ClipID == "" {
			return fmt.Errorf("scene: clip %d empty clip_id", i)
		}
		if ids[c.ClipID] {
			return fmt.Errorf("scene: duplicate clip_id %q", c.ClipID)
		}
		ids[c.ClipID] = true
		if c.DurMs <= 0 {
			return fmt.Errorf("scene: clip %s dur_ms %d <= 0", c.ClipID, c.DurMs)
		}
		for _, b := range c.Beats {
			if b.AtMs < 0 || b.AtMs > c.DurMs {
				return fmt.Errorf("scene: clip %s beat at_ms %d out of range", c.ClipID, b.AtMs)
			}
			if b.Kind == "line" && b.LineID != "" && s.line(b.LineID) == nil {
				return fmt.Errorf("scene: clip %s beat 引用不存在的台词 %s", c.ClipID, b.LineID)
			}
		}
		if c.Pick < 0 || c.Pick >= max(1, len(c.Candidates)) {
			return fmt.Errorf("scene: clip %s pick %d out of range", c.ClipID, c.Pick)
		}
		for _, lid := range c.LineIDs {
			if s.line(lid) == nil {
				return fmt.Errorf("scene: clip %s 引用不存在的台词 %s", c.ClipID, lid)
			}
		}
	}
	if len(s.Order) != len(s.Clips) {
		return fmt.Errorf("scene: order 有 %d 项但 clips 有 %d", len(s.Order), len(s.Clips))
	}
	for _, id := range s.Order {
		if !ids[id] {
			return fmt.Errorf("scene: order 里未知 clip %q", id)
		}
	}
	lids := map[string]bool{}
	for i, l := range s.Script.Lines {
		if l.LineID == "" || l.RoleID == "" || l.Text == "" {
			return fmt.Errorf("scene: script line %d 缺 line_id/role_id/text", i)
		}
		if lids[l.LineID] {
			return fmt.Errorf("scene: duplicate line_id %q", l.LineID)
		}
		lids[l.LineID] = true
		if s.role(l.RoleID) == nil {
			return fmt.Errorf("scene: line %s 引用未知角色 %q", l.LineID, l.RoleID)
		}
	}
	return nil
}

// OrderedClips 按 order 返回 clips（写盘前已校验过 order 与 clips 一一对应）。
func (s *Scene) OrderedClips() []*Clip {
	out := make([]*Clip, 0, len(s.Order))
	for _, id := range s.Order {
		out = append(out, s.clip(id))
	}
	return out
}

// ClipStartMs 返回 clip 在 scene 时间轴上的起点。
func (s *Scene) ClipStartMs(clipID string) int {
	t := 0
	for _, c := range s.OrderedClips() {
		if c.ClipID == clipID {
			return t
		}
		t += c.DurMs
	}
	return -1
}

func (s *Scene) TotalMs() int {
	t := 0
	for _, c := range s.OrderedClips() {
		t += c.DurMs
	}
	return t
}

// ---------- 编辑操作 ----------

// ReorderClips 重排（newOrder 必须仍是同一集合的排列）。
func (s *Scene) ReorderClips(newOrder []string) error {
	if len(newOrder) != len(s.Order) {
		return fmt.Errorf("reorder: 长度不符")
	}
	cur := map[string]bool{}
	for _, id := range s.Order {
		cur[id] = true
	}
	for _, id := range newOrder {
		if !cur[id] {
			return fmt.Errorf("reorder: 未知 clip %q", id)
		}
		delete(cur, id)
	}
	if len(cur) != 0 {
		return fmt.Errorf("reorder: 缺 clips %v", cur)
	}
	s.Order = append([]string(nil), newOrder...)
	return nil
}

// SplitClip 在 clip 内 atMs 处拆成两个 clip；beats 按时间归属，line_ids 按 beat 归属。
func (s *Scene) SplitClip(clipID string, atMs int) error {
	c := s.clip(clipID)
	if c == nil {
		return fmt.Errorf("split: 无 clip %q", clipID)
	}
	if atMs <= 0 || atMs >= c.DurMs {
		return fmt.Errorf("split: 切点 %d 越界（clip 长 %d）", atMs, c.DurMs)
	}
	a := *c
	b := *c
	b.ClipID = s.nextClipID()
	b.DurMs = c.DurMs - atMs
	a.DurMs = atMs
	a.Beats, b.Beats, a.LineIDs, b.LineIDs = nil, nil, nil, nil
	lidSeen := map[string]bool{}
	putLine := func(dst *[]string, e BeatEvent) {
		if e.LineID != "" && !lidSeen[e.LineID] {
			lidSeen[e.LineID] = true
			*dst = append(*dst, e.LineID)
		}
	}
	for _, e := range c.Beats {
		if e.AtMs < atMs {
			a.Beats = append(a.Beats, e)
			putLine(&a.LineIDs, e)
		} else {
			e.AtMs -= atMs
			b.Beats = append(b.Beats, e)
			putLine(&b.LineIDs, e)
		}
	}
	// 状态：b 继承 draft 源；video 源的 t0/t1 随之换算
	if c.IsFinal() {
		b.T0Ms = c.T0Ms + atMs
		a.T1Ms = c.T0Ms + atMs
		b.T1Ms = c.T1Ms
	}
	a.Status, b.Status = c.Status, c.Status
	a.Stale, b.Stale = c.Stale, c.Stale
	idx := indexOf(s.Clips, c)
	s.Clips = append(s.Clips[:idx+1], append([]Clip{b}, s.Clips[idx+1:]...)...)
	s.Clips[idx] = a
	oi := indexOfStr(s.Order, clipID)
	s.Order = append(s.Order[:oi+1], append([]string{b.ClipID}, s.Order[oi+1:]...)...)
	return nil
}

func indexOf(clips []Clip, c *Clip) int {
	for i := range clips {
		if &clips[i] == c || clips[i].ClipID == c.ClipID {
			return i
		}
	}
	return -1
}
func indexOfStr(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

// ensureClipSeq 保证分配器起点越过所有现存 clip 序号（旧版数据没有
// next_clip_seq 字段时，从现有 clip 推断）。
func (s *Scene) ensureClipSeq() {
	if s.NextClipSeq < 1 {
		s.NextClipSeq = 1
	}
	for _, c := range s.Clips {
		var n int
		if _, err := fmt.Sscanf(c.ClipID, "c%d", &n); err == nil && n >= s.NextClipSeq {
			s.NextClipSeq = n + 1
		}
	}
}

// nextClipID 从单调分配器取新 id——分配过的序号永不复用。
func (s *Scene) nextClipID() string {
	s.ensureClipSeq()
	for {
		id := fmt.Sprintf("c%02d", s.NextClipSeq)
		s.NextClipSeq++
		if s.clip(id) == nil {
			return id
		}
	}
}

func (s *Scene) DeleteClip(clipID string) error {
	if s.clip(clipID) == nil {
		return fmt.Errorf("delete: 无 clip %q", clipID)
	}
	i := indexOf(s.Clips, s.clip(clipID))
	s.Clips = append(s.Clips[:i], s.Clips[i+1:]...)
	s.Order = append(s.Order[:indexOfStr(s.Order, clipID)],
		s.Order[indexOfStr(s.Order, clipID)+1:]...)
	return nil
}

func (s *Scene) DuplicateClip(clipID string) error {
	c := s.clip(clipID)
	if c == nil {
		return fmt.Errorf("duplicate: 无 clip %q", clipID)
	}
	dup := *c
	dup.ClipID = s.nextClipID()
	dup.Beats = append([]BeatEvent(nil), c.Beats...)
	dup.Candidates = append([]ClipSource(nil), c.Candidates...)
	dup.LineIDs = append([]string(nil), c.LineIDs...)
	dup.Status = "draft"
	dup.Stale = false
	i := indexOf(s.Clips, c)
	s.Clips = append(s.Clips[:i+1], append([]Clip{dup}, s.Clips[i+1:]...)...)
	s.Order = append(s.Order[:indexOfStr(s.Order, clipID)+1],
		append([]string{dup.ClipID}, s.Order[indexOfStr(s.Order, clipID)+1:]...)...)
	return nil
}

// TrimClip 改 clip 时长（draft）或素材出入点（video）。draft：直接改 dur_ms（不动 beats）。
func (s *Scene) TrimClip(clipID string, durMs int) error {
	c := s.clip(clipID)
	if c == nil {
		return fmt.Errorf("trim: 无 clip %q", clipID)
	}
	if durMs < 200 {
		return fmt.Errorf("trim: 时长过短")
	}
	if c.IsFinal() {
		// video 源：dur_ms = t1-t0；把出点压/放到 dur
		c.T1Ms = c.T0Ms + durMs
	} else {
		// draft 源：不能裁到台词 beat 结束之前，否则画面先于台词收尾
		maxEnd := 0
		for _, b := range c.Beats {
			if b.Kind == "line" && b.AtMs+b.DurMs > maxEnd {
				maxEnd = b.AtMs + b.DurMs
			}
		}
		if durMs < maxEnd {
			return fmt.Errorf("trim: %d ms 短于最后一句台词结束点 %d ms", durMs, maxEnd)
		}
	}
	c.DurMs = durMs
	return nil
}

// SetSource 把 clip 换成某个候选版本（video 文件或回到 draft）。
// path 为空且 kind=draft 时回到占位。候选不存在时追加进 candidates。
func (s *Scene) SetSource(clipID, kind, path string, t0, t1 int) error {
	c := s.clip(clipID)
	if c == nil {
		return fmt.Errorf("set_source: 无 clip %q", clipID)
	}
	if kind != "draft" && kind != "video" {
		return fmt.Errorf("set_source: kind 只能是 draft|video")
	}
	src := ClipSource{Kind: kind, Path: path}
	found := -1
	for i, cand := range c.Candidates {
		if cand.Kind == kind && cand.Path == path {
			found = i
			break
		}
	}
	if found < 0 {
		if len(c.Candidates) == 0 {
			c.Candidates = []ClipSource{{Kind: "draft"}}
		}
		c.Candidates = append(c.Candidates, src)
		found = len(c.Candidates) - 1
	}
	c.Pick = found
	if kind == "video" {
		c.Status = "final"
		c.T0Ms, c.T1Ms = t0, t1
		if t1 > t0 {
			c.DurMs = t1 - t0
		}
	} else {
		c.Status = "draft"
	}
	return nil
}

// UpdateLine 改台本行（稳定 line_id 不动）。只把引用它的 clips 标 stale。
func (s *Scene) UpdateLine(lineID, text, roleID, direction string) error {
	l := s.line(lineID)
	if l == nil {
		return fmt.Errorf("update_line: 无台词 %q", lineID)
	}
	if roleID != "" && s.role(roleID) == nil {
		return fmt.Errorf("update_line: 未知角色 %q", roleID)
	}
	if text != "" {
		l.Text = text
	}
	if roleID != "" {
		l.RoleID = roleID
	}
	l.Direction = direction
	for i := range s.Clips {
		for _, lid := range s.Clips[i].LineIDs {
			if lid == lineID {
				s.Clips[i].Stale = true
			}
		}
	}
	return nil
}

// AddLine 在 afterID（""=末尾）之后插入新台词。
func (s *Scene) AddLine(afterID, roleID, text, direction string) (string, error) {
	if s.role(roleID) == nil {
		return "", fmt.Errorf("add_line: 未知角色 %q", roleID)
	}
	l := dubbing.ScriptLine{LineID: s.nextLineID(), SceneID: s.SceneID,
		RoleID: roleID, Text: text, Direction: direction}
	if afterID == "" {
		s.Script.Lines = append(s.Script.Lines, l)
	} else {
		i := indexOfStr(func() []string {
			ids := make([]string, len(s.Script.Lines))
			for j, x := range s.Script.Lines {
				ids[j] = x.LineID
			}
			return ids
		}(), afterID)
		if i < 0 {
			return "", fmt.Errorf("add_line: after %q 不存在", afterID)
		}
		s.Script.Lines = append(s.Script.Lines[:i+1],
			append([]dubbing.ScriptLine{l}, s.Script.Lines[i+1:]...)...)
	}
	return l.LineID, nil
}

func (s *Scene) nextLineID() string {
	max := 0
	for _, l := range s.Script.Lines {
		var n int
		if _, err := fmt.Sscanf(l.LineID, "l%d", &n); err == nil && n > max {
			max = n
		}
	}
	for i := max + 1; ; i++ {
		id := fmt.Sprintf("l%03d", i)
		if s.line(id) == nil {
			return id
		}
	}
}

func (s *Scene) DeleteLine(lineID string) error {
	for i, l := range s.Script.Lines {
		if l.LineID == lineID {
			s.Script.Lines = append(s.Script.Lines[:i], s.Script.Lines[i+1:]...)
			for ci := range s.Clips {
				c := &s.Clips[ci]
				keep := c.LineIDs[:0]
				had := false
				for _, lid := range c.LineIDs {
					if lid == lineID {
						had = true
						continue
					}
					keep = append(keep, lid)
				}
				c.LineIDs = keep
				// 同步删掉引用该台词的 line beats——留着会放出已删除
				// 台词的音频，并让 Validate 抓不到孤儿引用。
				keepB := c.Beats[:0]
				for _, b := range c.Beats {
					if b.Kind == "line" && b.LineID == lineID {
						continue
					}
					keepB = append(keepB, b)
				}
				c.Beats = keepB
				if had {
					c.Stale = true
				}
			}
			return nil
		}
	}
	return fmt.Errorf("delete_line: 无台词 %q", lineID)
}

// ---------- 反馈 ----------

func (s *Scene) AddFeedback(f Feedback) (string, error) {
	if f.Text == "" && f.Voice == "" {
		return "", fmt.Errorf("feedback: text 和 voice 至少给一个")
	}
	if f.Kind == "" {
		f.Kind = "other"
	}
	f.Status = "open"
	if f.ID == "" {
		f.ID = fmt.Sprintf("f%03d", len(s.Feedback)+1)
		for s.feedback(f.ID) != nil {
			f.ID = "f" + f.ID[1:] + "x" // 防重名
		}
	}
	s.Feedback = append(s.Feedback, f)
	return f.ID, nil
}

func (s *Scene) feedback(id string) *Feedback {
	for i := range s.Feedback {
		if s.Feedback[i].ID == id {
			return &s.Feedback[i]
		}
	}
	return nil
}

func (s *Scene) SetFeedbackStatus(id, status, resolution string) error {
	f := s.feedback(id)
	if f == nil {
		return fmt.Errorf("feedback: 无 %q", id)
	}
	switch status {
	case "open", "doing", "done", "wontfix":
		f.Status = status
	default:
		return fmt.Errorf("feedback: 未知状态 %q", status)
	}
	f.Resolution = resolution
	return nil
}

// ---------- 序列化 ----------

func ParseScene(b []byte) (*Scene, error) {
	var s Scene
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("scene: invalid JSON: %w", err)
	}
	// 先 normalize 自愈旧数据（孤儿 beats、缺 candidates），再严格校验。
	s.normalize()
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Scene) sortedBeats() {
	for i := range s.Clips {
		sort.SliceStable(s.Clips[i].Beats, func(a, b int) bool {
			return s.Clips[i].Beats[a].AtMs < s.Clips[i].Beats[b].AtMs
		})
	}
}

func (s *Scene) normalize() {
	if s.Script.Version == 0 {
		s.Script.Version = 1
	}
	s.ensureClipSeq()
	s.sortedBeats()
	for i := range s.Clips {
		if len(s.Clips[i].Candidates) == 0 {
			s.Clips[i].Candidates = []ClipSource{{Kind: "draft"}}
		}
		// 清掉引用已删台词的孤儿 line beats（旧数据或外部编辑可能残留）
		keep := s.Clips[i].Beats[:0]
		for _, b := range s.Clips[i].Beats {
			if b.Kind == "line" && b.LineID != "" && s.line(b.LineID) == nil {
				continue
			}
			keep = append(keep, b)
		}
		s.Clips[i].Beats = keep
	}
}

// Marshal 规范输出（稳定缩进）。
func (s *Scene) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	return append(b, '\n'), err
}

// LineCueTimes 返回每句台词在 scene 时间轴上的起点（ms）与所属 clip，
// 由 clips 的 line beats 按 order 累加得到；未排入任何 clip 的行不出现。
func (s *Scene) LineCueTimes() map[string][2]int { // line_id -> [scene_ms, clipDurMsAt]
	out := map[string][2]int{}
	t := 0
	for _, c := range s.OrderedClips() {
		for _, b := range c.Beats {
			if b.Kind == "line" && b.LineID != "" {
				out[b.LineID] = [2]int{t + b.AtMs, c.DurMs}
			}
		}
		t += c.DurMs
	}
	return out
}

var _ = strings.TrimSpace // keep import if unused paths evolve
