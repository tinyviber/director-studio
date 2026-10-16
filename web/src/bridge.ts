// bridge.ts — 前端 ↔ Go 服务层的桥。
// MyGo 环境走生成的 mygo.ts 绑定；纯浏览器（vite dev / playwright）走 mock
// 实现——编辑操作在内存 scene 上跑，媒体地址为空，录音按钮禁用提示。
import { isMyGo } from "mygo-runtime";
import * as mygo from "./mygo";

// ---------- 类型（镜像 scene.go） ----------

export interface VoiceProfile { backend: string; preset?: string; params?: Record<string, unknown>; gain?: number }
export interface Role { id: string; name: string; color?: string; voice?: VoiceProfile }
export interface BeatEvent {
  at_ms: number; kind: "line" | "action" | "camera" | "sfx" | "note";
  line_id?: string; actor?: string; action?: string; dur_ms?: number;
  from?: number[]; to?: number[]; text?: string;
}
export interface ClipSource { kind: "draft" | "video"; path?: string }
export interface Clip {
  clip_id: string; title?: string; dur_ms: number; beats: BeatEvent[];
  line_ids?: string[]; candidates?: ClipSource[]; pick: number;
  status: "draft" | "final" | "approved"; stale?: boolean;
  t0_ms?: number; t1_ms?: number; note?: string; risk?: string;
}
export interface ScriptLine { line_id: string; scene_id?: string; role_id: string; text: string; direction?: string }
export interface Feedback {
  id: string; created: string; scene_ms: number;
  clip_id?: string; clip_ms?: number; line_id?: string; take_ref?: string;
  kind: string; text: string; voice?: string; voice_text?: string;
  status: "open" | "doing" | "done" | "wontfix"; resolution?: string;
}
export interface AudioState {
  take?: string; take_sig?: string; session?: string;
  timeline?: string; timeline_sig?: string;
  rendered?: string; rendered_sig?: string;
}
export interface Scene {
  version: number; rev?: number; scene_id: string; title: string;
  roles: Role[]; script: { version: number; lines: ScriptLine[] };
  clips: Clip[]; order: string[]; audio?: AudioState; feedback?: Feedback[];
  meta?: Record<string, any>;
}
export interface TimelineSegment {
  segment_id: string; line_id?: string; role_id?: string;
  script_text?: string; spoken_text: string; start_ms: number; end_ms: number;
  match_status: string; similarity: number; review_required?: boolean;
  review_reasons?: string[]; take_index?: number; selected: boolean;
  discarded?: boolean; reviewed?: boolean; edited?: boolean;
}
export interface Timeline {
  version: number; audio: string; duration_ms: number;
  segments: TimelineSegment[]; review_count: number;
  unmatched_lines?: { line_id: string; text: string; reason: string }[];
}
export interface Preset { id: string; name: string; nameZh?: string; hint?: string; params: Record<string, number> }

// ---------- 调用封装 ----------

interface Env<T = unknown> { ok: boolean; data?: T; error?: string }

function parse<T>(raw: string): T {
  const env = JSON.parse(raw) as Env<T>;
  if (!env.ok) throw new Error(env.error || "unknown error");
  return env.data as T;
}

const call = <T>(p: Promise<string>) => p.then((r) => parse<T>(r));

export interface SceneListItem { scene_id: string; title: string; dir: string; clips: number; lines: number }
export interface AnalyzeResult { session: string; timeline: string; segments: number; review_count: number; unmatched: number }
export interface RenderResult { rendered: string; status: string; lines: number; warnings: string[]; skipped: number }
export interface ModelStatus { state: string; detail?: string; dir?: string }

export interface Bridge {
  pickDirectory(t: string): Promise<{ path: string }>;
  pickFile(t: string, e: string): Promise<{ path: string }>;
  openWorkspace(d: string): Promise<{ dir: string; scenes: SceneListItem[] }>;
  newScene(w: string, t: string): Promise<{ dir: string; scene: Scene }>;
  loadScene(d: string): Promise<Scene>;
  saveScene(d: string, p: string): Promise<Scene>;
  genDraft(d: string): Promise<{ scene: Scene; warnings: string[] }>;
  sceneEdit(d: string, op: string): Promise<{ scene: Scene; applied: unknown }>;
  undo(d: string): Promise<Scene>;
  redo(d: string): Promise<Scene>;
  saveRecording(w: string, b64: string, n: string): Promise<{ path: string }>;
  analyze(d: string, take: string): Promise<AnalyzeResult>;
  renderScene(d: string): Promise<RenderResult>;
  loadTimeline(p: string): Promise<Timeline>;
  saveTimelineFile(p: string, pay: string): Promise<{ path: string }>;
  morphFile(p: string, pp: string): Promise<{ wav: string; elapsed_ms: number; in_hz?: number }>;
  presetList(): Promise<Preset[]>;
  modelStatus(): Promise<ModelStatus>;
  scanModels(): Promise<unknown[]>;
  setModel(d: string, v: string): Promise<unknown>;
  exportFeedback(d: string): Promise<{ json: string; md: string }>;
  readFileB64(p: string): Promise<{ b64: string; bytes: number }>;
  loadTextFile(p: string): Promise<{ text: string }>;
  saveTextFile(p: string, pay: string): Promise<unknown>;
  openPath(p: string): Promise<unknown>;
  confirm(t: string, c: string): Promise<{ ok: boolean }>;
  scanVideos(d: string): Promise<string[]>;
  mediaBase(): Promise<{ base: string }>;
}

// 真实桥：调生成的绑定。方法名与 Go 绑定一致。
const real: Bridge = {
  pickDirectory: (t) => call(mygo.Studio.pickDirectory(t)),
  pickFile: (t, e) => call(mygo.Studio.pickFile(t, e)),
  openWorkspace: (d) => call(mygo.Studio.openWorkspace(d)),
  newScene: (w, t) => call(mygo.Studio.newScene(w, t)),
  loadScene: (d) => call(mygo.Studio.loadScene(d)),
  saveScene: (d, p) => call(mygo.Studio.saveScene(d, p)),
  genDraft: (d) => call(mygo.Studio.genDraft(d)),
  sceneEdit: (d, op) => call(mygo.Studio.sceneEdit(d, op)),
  undo: (d) => call(mygo.Studio.undo(d)),
  redo: (d) => call(mygo.Studio.redo(d)),
  saveRecording: (w, b64, n) => call(mygo.Studio.saveRecording(w, b64, n)),
  analyze: (d, take) => call(mygo.Studio.analyze(d, take)),
  renderScene: (d) => call(mygo.Studio.renderScene(d)),
  loadTimeline: (p) => call(mygo.Studio.loadTimeline(p)),
  saveTimelineFile: (p, pay) => call(mygo.Studio.saveTimelineFile(p, pay)),
  morphFile: (p, pp) => call(mygo.Studio.morphFile(p, pp)),
  presetList: () => call(mygo.Studio.presetList()),
  modelStatus: () => call(mygo.Studio.modelStatus()),
  scanModels: () => call(mygo.Studio.scanModels()),
  setModel: (d, v) => call(mygo.Studio.setModel(d, v)),
  exportFeedback: (d) => call(mygo.Studio.exportFeedback(d)),
  readFileB64: (p) => call(mygo.Studio.readFileB64(p)),
  loadTextFile: (p) => call(mygo.Studio.loadTextFile(p)),
  saveTextFile: (p, pay) => call(mygo.Studio.saveTextFile(p, pay)),
  openPath: (p) => call(mygo.Studio.openPath(p)),
  confirm: (t, c) => call(mygo.Studio.confirm(t, c)),
  scanVideos: (d) => call(mygo.Studio.scanVideos(d)),
  mediaBase: () => call(mygo.Studio.mediaBase()),
};

// ---------- mock（浏览器 dev / e2e UI 验证） ----------

let mockScene: Scene = {
  version: 1, scene_id: "demo", title: "示例场景 · 门口的异动",
  roles: [
    { id: "photo", name: "摄影师", color: "#4a90d9", voice: { backend: "dsp", preset: "normal" } },
    { id: "wukong", name: "悟空", color: "#d9634a", voice: { backend: "dsp", preset: "bright" } },
  ],
  script: {
    version: 1,
    lines: [
      { line_id: "l01", role_id: "photo", text: "巷子里怎么这么安静。" },
      { line_id: "l02", role_id: "wukong", text: "（蹲在墙头）师父说今晚要拍的就是这儿。" },
      { line_id: "l03", role_id: "photo", text: "你先下来，我去拿灯。" },
      { line_id: "l04", role_id: "wukong", text: "（跳下）灯不够亮的话，我可以变个大的。" },
      { line_id: "l05", role_id: "photo", text: "（回头）等等，那是什么声音？" },
      { line_id: "l06", role_id: "wukong", text: "（警觉）别动，有人在里面。" },
      { line_id: "l07", role_id: "photo", text: "（压低声音）要不要先报警？" },
      { line_id: "l08", role_id: "wukong", text: "不用，看我的。" },
    ],
  },
  clips: [], order: [],
};
// mock 的 draft：8 句 → 2 clips（本地版，与 Go GenDraft 简化对齐）
function mockGenDraft(s: Scene) {
  const lineDur = (t: string) => Math.max(700, Math.round(t.length / 4.2 * 1000) + 450);
  const clips: Clip[] = [];
  let beats: BeatEvent[] = [
    { at_ms: 0, kind: "camera", text: "wide" },
    { at_ms: 0, kind: "action", actor: "photo", action: "enter", to: [0.34, 0.82] },
    { at_ms: 0, kind: "action", actor: "wukong", action: "enter", to: [0.66, 0.82] },
  ];
  let lines: string[] = [];
  let dur = 600;
  let idx = 0;
  const flush = () => {
    if (!lines.length) return;
    clips.push({
      clip_id: `c${String(idx + 1).padStart(2, "0")}`, dur_ms: dur + 500,
      beats, line_ids: lines, candidates: [{ kind: "draft" }], pick: 0, status: "draft",
      title: s.script.lines.find((l) => l.line_id === lines[0])?.text.slice(0, 8) + "…",
    });
    idx++; beats = [{ at_ms: 0, kind: "camera", text: "med" }]; lines = []; dur = 0;
  };
  for (const l of s.script.lines) {
    const d = lineDur(l.text);
    if (lines.length && dur + d > 9000) flush();
    beats.push({ at_ms: dur, kind: "line", line_id: l.line_id, actor: l.role_id, dur_ms: d });
    beats.push({ at_ms: dur, kind: "action", actor: l.role_id, action: "talk", dur_ms: d });
    if (l.direction?.includes("跳下")) beats.push({ at_ms: dur, kind: "action", actor: l.role_id, action: "walk", to: [0.5, 0.85] });
    if (l.direction?.includes("回头")) beats.push({ at_ms: dur, kind: "action", actor: l.role_id, action: "turn" });
    if (l.direction?.includes("警觉")) beats.push({ at_ms: dur, kind: "action", actor: l.role_id, action: "surprise" });
    lines.push(l.line_id); dur += d;
  }
  flush();
  return clips;
}

const mockOps: Record<string, (s: Scene, op: any) => void> = {
  reorder(s, op) { s.order = op.order; },
  delete(s, op) { s.clips = s.clips.filter((c) => c.clip_id !== op.clip_id); s.order = s.order.filter((x) => x !== op.clip_id); },
  duplicate(s, op) {
    const c = s.clips.find((x) => x.clip_id === op.clip_id)!;
    const d = { ...c, clip_id: `c${Date.now() % 1000}`, status: "draft" as const };
    s.clips.splice(s.clips.indexOf(c) + 1, 0, d);
    s.order.splice(s.order.indexOf(op.clip_id) + 1, 0, d.clip_id);
  },
  split(s, op) {
    const c = s.clips.find((x) => x.clip_id === op.clip_id)!;
    const at = op.at_ms;
    const b: Clip = { ...c, clip_id: `c${Date.now() % 1000}`, dur_ms: c.dur_ms - at, beats: [], line_ids: [] };
    c.dur_ms = at;
    b.beats = c.beats.filter((e) => e.at_ms >= at).map((e) => ({ ...e, at_ms: e.at_ms - at }));
    c.beats = c.beats.filter((e) => e.at_ms < at);
    b.line_ids = b.beats.filter((e) => e.line_id).map((e) => e.line_id!);
    c.line_ids = c.beats.filter((e) => e.line_id).map((e) => e.line_id!);
    s.clips.splice(s.clips.indexOf(c) + 1, 0, b);
    s.order.splice(s.order.indexOf(c.clip_id) + 1, 0, b.clip_id);
  },
  trim(s, op) { s.clips.find((x) => x.clip_id === op.clip_id)!.dur_ms = op.dur_ms; },
  set_source(s, op) {
    const c = s.clips.find((x) => x.clip_id === op.clip_id)!;
    c.candidates = c.candidates || [{ kind: "draft" }];
    c.candidates.push({ kind: op.kind, path: op.path });
    c.pick = c.candidates.length - 1;
    c.status = op.kind === "video" ? "final" : "draft";
    if (op.kind === "video" && op.t1_ms > op.t0_ms) c.dur_ms = op.t1_ms - op.t0_ms;
  },
  update_line(s, op) {
    const l = s.script.lines.find((x) => x.line_id === op.line_id)!;
    if (op.text) l.text = op.text;
    if (op.role_id) l.role_id = op.role_id;
    l.direction = op.direction;
    s.clips.forEach((c) => { if (c.line_ids?.includes(op.line_id)) c.stale = true; });
  },
  add_line(s, op) {
    const i = op.after_id ? s.script.lines.findIndex((x) => x.line_id === op.after_id) : s.script.lines.length - 1;
    s.script.lines.splice(i + 1, 0, { line_id: `l${Date.now() % 10000}`, role_id: op.role_id, text: op.text, direction: op.direction });
  },
  delete_line(s, op) {
    s.script.lines = s.script.lines.filter((x) => x.line_id !== op.line_id);
    s.clips.forEach((c) => {
      if (c.line_ids?.includes(op.line_id)) { c.stale = true; c.line_ids = c.line_ids!.filter((x) => x !== op.line_id); }
      c.beats = c.beats.filter((b) => !(b.kind === "line" && b.line_id === op.line_id));
    });
  },
  add_feedback(s, op) {
    s.feedback = s.feedback || [];
    s.feedback.push({ id: `f${s.feedback.length + 1}`, created: new Date().toISOString(), status: "open", ...op });
  },
  feedback_status(s, op) { s.feedback!.find((f) => f.id === op.id)!.status = op.status; },
  set_role_voice(s, op) { s.roles.find((r) => r.id === op.role_id)!.voice = { backend: "dsp", preset: op.preset }; },
  note(s, op) { s.clips.find((x) => x.clip_id === op.clip_id)!.note = op.text; },
  set_take(s, op) { s.audio = { ...(s.audio ?? {}), take: op.path }; },
};

const mockTimeline: Timeline = {
  version: 1, audio: "take.wav", duration_ms: 12000, review_count: 1,
  segments: [
    { segment_id: "seg-0001", line_id: "l01", role_id: "photo", script_text: "巷子里怎么这么安静。", spoken_text: "巷子里怎么这么安静", start_ms: 500, end_ms: 2500, match_status: "matched", similarity: 0.93, selected: true },
    { segment_id: "seg-0002", line_id: "", spoken_text: "广告时间到", start_ms: 3000, end_ms: 4000, match_status: "unmatched", similarity: 0, review_required: true, review_reasons: ["no matching script line"], selected: false },
    { segment_id: "seg-0003", line_id: "l02", role_id: "wukong", script_text: "（蹲在墙头）师父说今晚要拍的就是这儿。", spoken_text: "师父说今晚要拍的就是这儿", start_ms: 5000, end_ms: 7200, match_status: "matched", similarity: 0.8, selected: true },
  ],
  unmatched_lines: [{ line_id: "l04", text: "（跳下）灯不够亮的话，我可以变个大的。", reason: "no_audio" }],
};

const mock: Bridge = {
      pickDirectory: async () => ({ path: "/mock/workspace" }),
      pickFile: async () => ({ path: "/mock/clip.mp4" }),
      openWorkspace: async (d: string) => ({ dir: d, scenes: [{ scene_id: "demo", title: mockScene.title, dir: d + "/scenes/demo", clips: mockScene.clips.length, lines: mockScene.script.lines.length }] }),
      newScene: async () => ({ dir: "/mock/workspace/scenes/demo", scene: mockScene }),
      loadScene: async () => { if (!mockScene.clips.length) { mockScene.clips = mockGenDraft(mockScene); mockScene.order = mockScene.clips.map((c) => c.clip_id); } return structuredClone(mockScene); },
      saveScene: async (_d: string, p: string) => { mockScene = JSON.parse(p); return mockScene; },
      genDraft: async () => { mockScene.clips = mockGenDraft(mockScene); mockScene.order = mockScene.clips.map((c) => c.clip_id); return { scene: mockScene, warnings: [] }; },
      sceneEdit: async (_d: string, opS: string) => { const op = JSON.parse(opS); mockOps[op.op]?.(mockScene, op); return { scene: structuredClone(mockScene), applied: op }; },
      undo: async () => mockScene,
      redo: async () => mockScene,
      saveRecording: async () => ({ path: "/mock/take.wav" }),
      analyze: async () => ({ session: "/mock/session", timeline: "/mock/timeline.json", segments: 3, review_count: 1, unmatched: 1 }),
      renderScene: async () => ({ rendered: "/mock/rendered", status: "partial", lines: 3, warnings: [], skipped: 1 }),
      loadTimeline: async () => structuredClone(mockTimeline),
      saveTimelineFile: async () => ({ path: "/mock/timeline.json" }),
      morphFile: async () => ({ wav: "", elapsed_ms: 12 }),
      presetList: async () => ([
        { id: "normal", name: "Normal", nameZh: "原声", params: {} },
        { id: "woman", name: "Woman", nameZh: "女声", params: {} },
        { id: "child", name: "Child", nameZh: "小孩", params: {} },
        { id: "demon", name: "Demon", nameZh: "恶魔", params: {} },
      ] as Preset[]),
      modelStatus: async () => ({ state: "missing", detail: "浏览器 mock" }),
      scanModels: async () => [],
      setModel: async () => ({}),
      exportFeedback: async () => ({ json: "/mock/fb.json", md: "/mock/fb.md" }),
      readFileB64: async () => ({ b64: "", bytes: 0 }),
      loadTextFile: async () => ({ text: "" }),
      saveTextFile: async () => ({}),
      openPath: async () => ({}),
      confirm: async () => ({ ok: true }),
      scanVideos: async () => ["/mock/final/c01.mp4", "/mock/final/c02.mp4"],
      mediaBase: async () => ({ base: "" }),
    };

export const bridge: Bridge = isMyGo() ? real : mock;
export const isReal = isMyGo();
