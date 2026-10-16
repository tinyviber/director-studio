// draft.go — 台本 → Draft Animatic spec 生成器。
//
// 目标不是生成精美画面，而是生成"叙事成立"的可连续播放 spec：
//   - 台词按语速估计时长（CHARS_PER_SEC），聚合成 ≤MaxClipMs 的 clip；
//     连续事件可跨 clip（line 长度本身可超 10s，聚合时先整句放入）。
//   - 方向词（direction / text）映射到参数化基础动作：
//     stand|sit|walk|point|talk|turn|surprise|smile|sad|nod|shake|enter|exit|idle。
//   - 镜头默认 wide，长台词 clip 内交替给说话人中景（med:<role>），
//     形成剪辑节奏；切场由台本 direction 关键词触发（cut/fade/black）。
//   - 每个 clip 的 beats 都带 at_ms；播放端按真实时钟驱动，不靠逐帧猜。
//
// 生成的 draft 是可重放的数据——Draft 质量标准是保留空间关系、因果顺序、
// 反应时机、镜头节奏和声画相对时间，不是纹理与光影。
package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// CharsPerSec 中文朗读经验语速（字/秒）；用于估算台词时长。
	CharsPerSec = 4.2
	// MaxClipMs 生成 clip 的软上限（不超过 10s 的制作单位）。
	MaxClipMs = 9000
	// MinClipMs 过短 clip 的下限（避免碎成一堆半秒镜头）。
	MinClipMs = 1200
	// LinePadMs 每句台词后的喘息/停顿。
	LinePadMs = 450
	// ClipTailMs clip 结尾留白。
	ClipTailMs = 500
)

// dirAction 关键词 → 基础动作。复杂运动先用参数化姿态+轨迹近似。
var dirAction = []struct {
	kw, action string
}{
	{"走", "walk"}, {"跑", "walk"}, {"移动", "walk"}, {"后退", "walk"}, {"上前", "walk"},
	{"坐", "sit"}, {"站起", "stand"}, {"站", "stand"},
	{"指", "point"}, {"伸手", "point"}, {"递", "point"}, {"拿", "point"}, {"举", "point"},
	{"回头", "turn"}, {"转身", "turn"}, {"转", "turn"},
	{"惊", "surprise"}, {"吓", "surprise"}, {"慌", "surprise"},
	{"笑", "smile"}, {"乐", "smile"},
	{"难过", "sad"}, {"哭", "sad"}, {"怒", "sad"}, {"气", "sad"},
	{"点头", "nod"}, {"摇头", "shake"},
	{"停", "idle"}, {"沉默", "idle"}, {"顿", "idle"},
	{"进", "enter"}, {"入场", "enter"}, {"出场", "exit"}, {"退", "exit"},
}

var dirCamera = []struct{ kw, cam string }{
	{"全景", "wide"}, {"远景", "wide"}, {"中景", "med"}, {"近景", "cu"}, {"特写", "cu"},
	{"推", "push"}, {"拉", "pull"}, {"跟", "follow"}, {"切", "cut"}, {"淡入", "fadein"}, {"淡出", "fadeout"}, {"黑场", "black"},
}

// lineDurMs 一句台词的估算时长（时长主要由真实录音校准；无录音时用估算）。
func lineDurMs(text string) int {
	n := utf8.RuneCountInString(strings.TrimSpace(text))
	ms := int(float64(n)/CharsPerSec*1000) + LinePadMs
	if ms < 700 {
		ms = 700
	}
	return ms
}

// actorLayout 按出现顺序给角色摆位：2 人时左右分居，多人扇形展开。
func actorLayout(n int, i int) []float64 {
	if n <= 1 {
		return []float64{0.5, 0.82}
	}
	if n == 2 {
		return [][]float64{{0.34, 0.82}, {0.66, 0.82}}[i%2]
	}
	// 3+：x 均匀摊开
	x := 0.18 + 0.64*float64(i)/float64(n-1)
	return []float64{x, 0.82}
}

// parseDirBeats 从 direction 文本抽出动作/镜头事件（可多个，按出现顺序）。
func parseDirBeats(dir string) (actions []string, cams []string) {
	for _, a := range dirAction {
		if strings.Contains(dir, a.kw) {
			actions = append(actions, a.action)
		}
	}
	for _, c := range dirCamera {
		if strings.Contains(dir, c.kw) {
			cams = append(cams, c.cam)
		}
	}
	return
}

// GenDraft 把台本切成 clips 并生成 beats。preserve 可传旧 clips，把已有
// source/status/pick 迁到 line_ids 完全相同的 clip 上（升级 Final 的素材绑定
// 不因重排/重生成丢失）。
func GenDraft(scene *Scene, preserve []Clip) ([]Clip, []string, error) {
	if len(scene.Script.Lines) == 0 {
		return nil, nil, fmt.Errorf("gen_draft: 台本为空")
	}
	warns := []string{}
	type prevMap map[string]Clip // key = join(line_ids)
	// 稳定身份：所有旧 clip_id 都保留。line_ids 一致的 clip 沿用旧 id
	// （批注/素材引用不失效）；没被复用的旧 id 也不再分配给新内容——
	// 指向它们的批注会悬空并可被识别，而不是静默指到别的素材上。
	used := map[string]bool{}
	prev := prevMap{}
	// 同一 line_ids 组合可能有多个旧 clip（比如 duplicate 出的副本）：
	// 有正式视频绑定的优先，其次非 draft 状态，其余按先出现。
	better := func(a, b Clip) bool {
		if a.IsFinal() != b.IsFinal() {
			return a.IsFinal()
		}
		if a.Status != b.Status {
			return a.Status != "draft"
		}
		return false
	}
	for _, c := range preserve {
		k := strings.Join(c.LineIDs, "|")
		if old, ok := prev[k]; !ok || better(c, old) {
			prev[k] = c
		}
		used[c.ClipID] = true
	}
	// 新 id 来自 scene 的单调分配器：本次之外被淘汰过的序号也不会复用。
	scene.ensureClipSeq()
	allocID := func() string {
		for {
			id := fmt.Sprintf("c%02d", scene.NextClipSeq)
			scene.NextClipSeq++
			if !used[id] {
				used[id] = true
				return id
			}
		}
	}

	// 角色顺序 = 台本中首次出现顺序
	roleOrder := []string{}
	for _, l := range scene.Script.Lines {
		found := false
		for _, r := range roleOrder {
			if r == l.RoleID {
				found = true
				break
			}
		}
		if !found {
			roleOrder = append(roleOrder, l.RoleID)
		}
	}
	if len(roleOrder) == 0 {
		return nil, nil, fmt.Errorf("gen_draft: 无角色台词")
	}
	posOf := map[string]int{}
	for i, r := range roleOrder {
		posOf[r] = i
	}

	clips := []Clip{}
	curBeats := []BeatEvent{}
	curLines := []string{}
	curDur := 0
	mkClip := func() {
		if len(curBeats) == 0 {
			return
		}
		dur := curDur + ClipTailMs
		if dur < MinClipMs {
			dur = MinClipMs
		}
		c := Clip{
			DurMs:      dur,
			Beats:      curBeats,
			LineIDs:    curLines,
			Status:     "draft",
			Candidates: []ClipSource{{Kind: "draft"}},
		}
		// 迁移旧 clip 的身份与全部审核状态（line_ids 一致说明叙事内容
		// 没变）：candidates/pick/status/stale/出入点/标题/备注/风险。
		if old, ok := prev[strings.Join(curLines, "|")]; ok {
			c.ClipID = old.ClipID
			c.Candidates = old.Candidates
			c.Pick = old.Pick
			c.Status = old.Status
			c.Stale = old.Stale
			c.T0Ms, c.T1Ms = old.T0Ms, old.T1Ms
			c.Title = old.Title
			c.Note = old.Note
			c.Risk = old.Risk
		} else {
			c.ClipID = allocID()
		}
		clips = append(clips, c)
		curBeats, curLines, curDur = nil, nil, 0
	}

	// 开场镜头：wide + 所有在场角色站位 + 入场
	open := BeatEvent{AtMs: 0, Kind: "camera", Text: "wide"}
	curBeats = append(curBeats, open)
	for i, r := range roleOrder {
		curBeats = append(curBeats, BeatEvent{
			AtMs: 0, Kind: "action", Actor: r, Action: "enter",
			To: actorLayout(len(roleOrder), i),
		})
	}
	curDur += 600

	lastSpeaker := ""
	medTurn := 0
	for _, l := range scene.Script.Lines {
		lDur := lineDurMs(l.Text)
		// 该句进来会不会撑爆当前 clip？当前已有内容且超限则收镜开新 clip
		if len(curBeats) > 0 && curDur+lDur > MaxClipMs && len(curLines) > 0 {
			mkClip()
			// 新 clip 重新建立镜头（连戏保持机位：说话人不变则补一个 med）
			curBeats = append(curBeats, BeatEvent{AtMs: 0, Kind: "camera",
				Text: cameraFor(roleOrder, posOf[l.RoleID], medTurn)})
		}
		curLines = append(curLines, l.LineID)
		curBeats = append(curBeats, BeatEvent{
			AtMs: curDur, Kind: "line", LineID: l.LineID, Actor: l.RoleID, DurMs: lDur,
		})
		// 台词同时给说话人一个 talk 动作；换说话人时给镜头（中景节奏）
		curBeats = append(curBeats, BeatEvent{
			AtMs: curDur, Kind: "action", Actor: l.RoleID, Action: "talk", DurMs: lDur,
		})
		if l.RoleID != lastSpeaker {
			curBeats = append(curBeats, BeatEvent{
				AtMs: curDur, Kind: "camera",
				Text: cameraFor(roleOrder, posOf[l.RoleID], medTurn),
			})
			lastSpeaker = l.RoleID
			medTurn++
		}
		// direction → 动作/镜头事件（在台词起点附近展开）。
		// 只解析导演明确写的 direction，不猜台词正文——"推进工作""进入
		// 状态"这类普通对白不该意外触发镜头和动作。
		acts, cams := parseDirBeats(l.Direction)
		for i, a := range acts {
			curBeats = append(curBeats, BeatEvent{
				AtMs: curDur + min(200*i, lDur-1), Kind: "action",
				Actor: l.RoleID, Action: a, DurMs: min(1200, lDur),
			})
		}
		for _, cm := range cams {
			curBeats = append(curBeats, BeatEvent{
				AtMs: curDur, Kind: "camera", Text: cm,
			})
		}
		curDur += lDur
	}
	mkClip()

	if len(clips) == 0 {
		return nil, nil, fmt.Errorf("gen_draft: 未生成任何 clip")
	}
	// 生成标题（每 clip 取首句前 8 字）
	for i := range clips {
		if clips[i].Title == "" && len(clips[i].LineIDs) > 0 {
			if l := scene.line(clips[i].LineIDs[0]); l != nil {
				r := []rune(l.Text)
				if len(r) > 8 {
					r = r[:8]
				}
				clips[i].Title = fmt.Sprintf("%s…", string(r))
			}
		}
	}
	return clips, warns, nil
}

// cameraFor 轮换中景策略：同一说话人连续多句时，在 med 和 cu 之间交替。
func cameraFor(roleOrder []string, pos, turn int) string {
	if len(roleOrder) <= 1 {
		return "cu"
	}
	if turn%3 == 2 {
		return "cu"
	}
	return "med"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
