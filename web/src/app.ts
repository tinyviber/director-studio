// app.ts — 导演工作台主装配：workspace/scene 状态、四个面板、播放器、时间线。
import { bridge, isReal, type Scene, type Clip, type Timeline, type ScriptLine, type Preset } from "./bridge";
import { Player } from "./player";
import { renderVideoLane, renderAudioLane } from "./timeline";
import { Recorder } from "./recorder";

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;

const state = {
  ws: "",            // workspace dir
  sceneDir: "",      // scenes/<id>/
  scene: null as Scene | null,
  selClip: "" as string,      // 当前选中 clip_id
  timeline: null as Timeline | null,
  presets: [] as Preset[],
  dirty: false,
};

const player = new Player($("cv") as unknown as HTMLCanvasElement, $("vid") as unknown as HTMLVideoElement);
const rec = new Recorder();
const toast = (msg: string) => {
  const t = document.createElement("div");
  t.className = "toast"; t.textContent = msg;
  document.body.appendChild(t);
  setTimeout(() => t.remove(), 2600);
};
const setSave = (s: string) => { $("save-state").textContent = s; };

const micIcon = `<svg viewBox="0 0 16 16" width="12" height="12" style="vertical-align:-1.5px"><rect x="5.5" y="2" width="5" height="8" rx="2.5" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M3 8a5 5 0 0 0 10 0M8 13v2" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>`;
const stopIcon = `<svg viewBox="0 0 16 16" width="10" height="10" style="vertical-align:-1px"><rect x="4" y="4" width="8" height="8" rx="1.5" fill="currentColor"/></svg>`;
const playSVG = `<path d="M5 3.5v9l7-4.5z" fill="currentColor"/>`;
const pauseSVG = `<path d="M4.5 3h3v10h-3zM8.5 3h3v10h-3z" fill="currentColor"/>`;

function statusBadge(status: string, stale?: boolean): string {
  const cls = status === "final" ? "ok" : status === "approved" ? "acc" : "warn";
  const label = { draft: "草稿", final: "成片", approved: "已批准" }[status] ?? status;
  return `<span class="badge ${cls}">${label}</span>` +
    (stale ? ` <span class="badge err">已过期</span>` : "");
}

function fmt(ms: number) {
  const s = ms / 1000, m = Math.floor(s / 60);
  return `${m}:${(s % 60).toFixed(1).padStart(4, "0")}`;
}

// ---------- 场景载入 ----------

// applyScene 把新场景文档同步到播放器。preserve=true（同场景编辑/undo）
// 保留播放头与音频缓存；false（换场景）全量重置。
function applyScene(s: Scene, preserve: boolean) {
  state.scene = s;
  player.setScene(s, preserve);
  player.timeline = state.timeline;
  // manifest 到位后刷一次音频 lane，让"无音"徽标反映真实 rendered 产物
  if (s.audio?.rendered) void player.loadManifest().then(() => renderTimeline());
}

async function loadScene(dir: string) {
  state.sceneDir = dir;
  const mb = await bridge.mediaBase().catch(() => ({ base: "", key: "" }));
  player.mediaBase = mb.base;
  player.mediaKey = mb.key ?? "";
  const s = await bridge.loadScene(dir);
  if (s.audio?.timeline) {
    state.timeline = await bridge.loadTimeline(s.audio.timeline).catch(() => null);
  } else {
    state.timeline = null;
  }
  applyScene(s, false);
  refreshAll();
  setSave("已载入 " + s.scene_id);
}

// 编辑串行化：每个 SceneEdit 服务端各自 load→改→save，并发提交会
// 互相覆盖（lost update）。进队列排队，一次一个在途。
let editChain: Promise<unknown> = Promise.resolve();
function enqueue<T>(fn: () => Promise<T>): Promise<T> {
  const p = editChain.then(fn);
  editChain = p.then(() => undefined, () => undefined);
  return p;
}

// edit 返回是否成功——调用方据此决定能不能清空输入/提示完成。
async function edit(op: Record<string, unknown>): Promise<boolean> {
  if (!state.sceneDir) return false;
  return enqueue(async () => {
    try {
      const r = await bridge.sceneEdit(state.sceneDir,
        JSON.stringify({ ...op, base_rev: state.scene?.rev ?? 0 })) as any;
      if (r.scene) applyScene(r.scene, true);
      refreshAll();
      setSave("已保存 ✓");
      return true;
    } catch (e) {
      const msg = String(e);
      // 版本冲突：别人（含 agent）改过文件——刷新后让用户重试，不静默覆盖。
      if (msg.includes("已被修改")) {
        try { applyScene(await bridge.loadScene(state.sceneDir), true); refreshAll(); } catch { /* ignore */ }
        toast("场景已被外部修改，已刷新——请重试");
      } else {
        toast(msg);
      }
      return false;
    }
  });
}

// ---------- 台本面板 ----------

function renderScript() {
  const box = $("script-lines");
  box.innerHTML = "";
  const s = state.scene;
  if (!s) { box.innerHTML = `<div class="row small" style="padding:12px">先打开工作目录并选择/新建场景。</div>`; return; }
  const cues = player.lineCue();
  const fbLines = new Set((s.feedback ?? []).filter((f) => f.status === "open" || f.status === "doing").map((f) => f.line_id));
  for (const l of s.script.lines) {
    const d = document.createElement("div");
    d.className = "sline";
    d.dataset.lid = l.line_id;
    const role = s.roles.find((r) => r.id === l.role_id);
    const flags = `${cues.has(l.line_id) ? `<svg viewBox="0 0 16 16" width="9" height="9"><path d="M5 4v8l6-4z" fill="currentColor"/></svg>` : ""}${fbLines.has(l.line_id) ? `<span class="fb-dot"></span>` : ""}`;
    d.innerHTML = `
      <div class="meta">
        <span class="lid">${esc(l.line_id)}</span>
        <span class="role" ${role?.color ? `style="background:${role.color}22;color:${role.color}"` : ""}>${esc(role?.name ?? l.role_id)}</span>
        <span class="flags">${flags}</span>
      </div>
      <div class="txt">${esc(l.text)}</div>
      ${l.direction ? `<div class="dir">${esc(l.direction)}</div>` : ""}`;
    d.addEventListener("click", (e) => {
      if ((e.target as HTMLElement).closest("button,textarea")) return;
      const ms = cues.get(l.line_id);
      if (ms !== undefined) void player.seek(ms);
      markActive(l.line_id);
    });
    d.addEventListener("dblclick", () => editLine(l));
    box.appendChild(d);
  }
}

function markActive(lid: string) {
  document.querySelectorAll(".sline.active").forEach((x) => x.classList.remove("active"));
  document.querySelector(`.sline[data-lid="${lid}"]`)?.classList.add("active");
}

function editLine(l: ScriptLine) {
  const d = document.querySelector(`.sline[data-lid="${l.line_id}"]`)!;
  if (d.classList.contains("editing")) return;
  d.classList.add("editing");
  const s = state.scene!;
  const ta = document.createElement("textarea");
  ta.rows = 2; ta.value = l.text;
  const dirIn = document.createElement("input");
  dirIn.placeholder = "动作/镜头说明（direction）"; dirIn.value = l.direction ?? "";
  const row = document.createElement("div");
  row.className = "edit-row";
  const roleSel = document.createElement("select");
  for (const r of s.roles) {
    const o = document.createElement("option");
    o.value = r.id; o.textContent = r.name;
    if (r.id === l.role_id) o.selected = true;
    roleSel.appendChild(o);
  }
  const ok = document.createElement("button"); ok.textContent = "存"; ok.className = "btn action";
  const del = document.createElement("button"); del.textContent = "删行"; del.className = "btn danger";
  ok.onclick = () => edit({ op: "update_line", line_id: l.line_id, text: ta.value, role_id: roleSel.value, direction: dirIn.value });
  del.onclick = async () => {
    const c = await bridge.confirm("删除台词", `删除 ${l.line_id}？引用它的 clip 会标记过期。`);
    if (c.ok) edit({ op: "delete_line", line_id: l.line_id });
  };
  row.append(roleSel, ok, del);
  d.append(ta, dirIn, row);
  ta.focus();
}

// ---------- 时间线 ----------

function renderTimeline() {
  const s = state.scene;
  if (!s) { $("tl-video").innerHTML = ""; $("tl-audio").innerHTML = ""; return; }
  renderVideoLane($("tl-video"), s, state.selClip, null, {
    onSelect: (id) => { state.selClip = id; renderProps(); renderTimeline(); },
    onSeekClip: (id) => { const ms = s.clips.length ? clipStart(s, id) : 0; void player.seek(ms); },
    onReorder: (order) => edit({ op: "reorder", order }),
    onPlayLine: (lid) => { const ms = player.lineCue().get(lid); if (ms !== undefined) void player.seek(ms); },
  });
  renderAudioLane($("tl-audio"), s, {
    onSelect: () => {}, onReorder: () => {},
    onSeekClip: () => {}, onPlayLine: playLineAudio,
  }, lineWavs());
}

function clipStart(s: Scene, clipID: string) {
  let t = 0;
  for (const id of s.order) {
    if (id === clipID) return t;
    t += s.clips.find((x) => x.clip_id === id)?.dur_ms ?? 0;
  }
  return 0;
}

function lineWavs(): Map<string, string> {
  const m = new Map<string, string>();
  if (state.timeline) {
    for (const seg of state.timeline.segments) {
      if (seg.line_id && seg.selected && state.scene?.audio?.take) m.set(seg.line_id, state.scene.audio.take);
    }
  }
  // rendered manifest 里已有 wav 的行也算有音（优先显示 rendered 路径）
  for (const l of state.scene?.script.lines ?? []) {
    const p = player.renderedWav(l.line_id);
    if (p) m.set(l.line_id, p);
  }
  return m;
}

async function playLineAudio(lid: string) {
  if (!(await player.playLineOnce(lid))) toast("这句还没有音频——先录音并对齐");
}

// ---------- 属性面板 ----------

function renderProps() {
  const box = $("clip-props");
  const s = state.scene;
  const c = s?.clips.find((x) => x.clip_id === state.selClip);
  if (!s || !c) { box.innerHTML = `<div class="row small">选中一个 clip 查看属性</div>`; return; }
  const src = c.candidates?.[c.pick ?? 0];
  const stBadge = statusBadge(c.status, c.stale);
  box.innerHTML = `
    <div class="i-row"><span class="k">Clip</span><span class="v">${esc(c.clip_id)} ${esc(c.title ?? "")}</span></div>
    <div class="i-row"><span class="k">时长</span><span class="v">${(c.dur_ms / 1000).toFixed(2)} s</span></div>
    <div class="i-row"><span class="k">状态</span><span class="v">${stBadge}</span></div>
    <div class="i-row"><span class="k">台词</span><span class="v">${(c.line_ids ?? []).map(esc).join(", ") || "—"}</span></div>
    <div class="i-row"><span class="k">当前源</span><span class="v">${src?.kind === "video" ? "视频 " + esc(src.path?.split("/").pop() ?? "") : "draft 火柴人"}</span></div>`;
  const list = document.createElement("div");
  (c.candidates ?? []).forEach((cand, i) => {
    const r = document.createElement("div");
    r.className = "cand" + (i === (c.pick ?? 0) ? " cur" : "");
    r.innerHTML = `<span>${cand.kind === "draft" ? "draft" : esc(cand.path?.split("/").pop() ?? "")}</span>`;
    const use = document.createElement("button");
    use.textContent = "用这版";
    use.onclick = () => edit({ op: "set_source", clip_id: c.clip_id, kind: cand.kind, path: cand.path ?? "", t0_ms: c.t0_ms ?? 0, t1_ms: c.t1_ms ?? 0 });
    r.appendChild(use);
    list.appendChild(r);
  });
  box.appendChild(list);
  const acts = document.createElement("div");
  acts.className = "actions";
  const mk = (label: string, fn: () => void, danger = false) => {
    const b = document.createElement("button"); b.textContent = label; b.onclick = fn; if (danger) b.className = "btn danger"; acts.appendChild(b);
  };
  mk("在此拆分", async () => {
    const at = player.clipAt(player.playhead);
    if (!at || at.clip.clip_id !== c.clip_id) {
      const local = await promptNum("在 clip 内第几 ms 拆分？", String(Math.floor(c.dur_ms / 2)));
      if (local == null) return;
      return edit({ op: "split", clip_id: c.clip_id, at_ms: local });
    }
    return edit({ op: "split", clip_id: c.clip_id, at_ms: at.local });
  });
  mk("复制", () => edit({ op: "duplicate", clip_id: c.clip_id }));
  mk("裁剪时长", async () => {
    const d = await promptNum("新时长 ms", String(c.dur_ms));
    if (d != null) edit({ op: "trim", clip_id: c.clip_id, dur_ms: d });
  });
  mk("绑定正式素材…", async () => {
    const f = await bridge.pickFile("选择成片视频", "mp4,webm,mov");
    if (f?.path) {
      const t0 = await promptNum("入点 ms", "0") ?? 0;
      const t1 = await promptNum("出点 ms（0=到尾）", "0") ?? 0;
      edit({ op: "set_source", clip_id: c.clip_id, kind: "video", path: f.path, t0_ms: t0, t1_ms: t1 });
    }
  });
  mk("删除", async () => {
    const ok = await bridge.confirm("删除 clip", `删除 ${c.clip_id}？可撤销。`);
    if (ok.ok) edit({ op: "delete", clip_id: c.clip_id });
  }, true);
  box.appendChild(acts);
}

function promptNum(label: string, def: string): Promise<number | null> {
  const v = window.prompt(label, def);
  if (v == null) return Promise.resolve(null);
  const n = parseInt(v, 10);
  return Promise.resolve(Number.isFinite(n) ? n : null);
}

// ---------- 配音面板 ----------

async function renderVoice() {
  const s = state.scene;
  $("take-list").innerHTML = "";
  if (!s) return;
  // takes: audio.take + rec 目录列不出（没 ls API）→ 显示当前 take + 提供"选文件"
  const audioStatus = (s.meta?.audio_status ?? {}) as
    { timeline_stale?: boolean; rendered_stale?: boolean };
  if (s.audio?.take) {
    const d = document.createElement("div");
    d.className = "take";
    // rendered_status：指纹一致不代表产物完整——partial/blocked_review/
    // failed 的 rendered 不进正式预览，这里必须显式标出来。
    const rs = s.audio.rendered_status ?? "";
    const rsBadge = s.audio.rendered && rs !== "" && rs !== "complete"
      ? ` <span class="badge err">渲染${rs === "failed" ? "失败" : rs === "partial" ? "不完整" : "待审核阻塞"}——不进预览</span>` : "";
    const staleBadges =
      (audioStatus.timeline_stale ? ` <span class="badge err">对齐已过期——换了录音，需重新 ASR</span>` : "") +
      (audioStatus.rendered_stale ? ` <span class="badge err">渲染已过期——需重新渲染配音</span>` : "") +
      rsBadge;
    d.innerHTML = `<svg viewBox="0 0 16 16" width="12" height="12" style="flex:none;color:var(--muted)"><rect x="5.5" y="2" width="5" height="8" rx="2.5" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M3 8a5 5 0 0 0 10 0M8 13v2" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg><span class="nm">${esc(s.audio.take.split("/").pop() ?? "")}${staleBadges}</span>`;
    const play = document.createElement("button"); play.textContent = "试听";
    play.onclick = () => {
      if (player.mediaBase) void new Audio(player.mediaURL(s.audio!.take!)).play();
    };
    const morph = document.createElement("button"); morph.textContent = "变声试听";
    morph.onclick = async () => {
      const pr = prompt("预设 id（normal/low/woman/child/demon…）", "woman");
      if (!pr) return;
      const r = await bridge.morphFile(s.audio!.take!, pr).catch((e) => { toast(String(e)); return null; });
      if (r?.wav) {
        const a = new Audio("data:audio/wav;base64," + r.wav);
        void a.play();
        toast(`变声渲染 ${r.elapsed_ms?.toFixed(0)}ms`);
      }
    };
    d.append(play, morph);
    $("take-list").appendChild(d);
  }
  $("btn-analyze").toggleAttribute("disabled", !s.audio?.take);
  $("btn-render").toggleAttribute("disabled", !s.audio?.timeline);
  // ASR 对齐表
  const tbl = $("asr-table");
  tbl.innerHTML = "";
  if (state.timeline) {
    for (const seg of state.timeline.segments) {
      const d = document.createElement("div");
      const cls = seg.match_status === "unmatched" ? "unmatched" : seg.review_required && !seg.reviewed ? "review" : "ok";
      d.className = `seg ${cls}`;
      d.innerHTML = `<div class="meta">${seg.segment_id} · ${(seg.start_ms / 1000).toFixed(1)}–${(seg.end_ms / 1000).toFixed(1)}s
          ${seg.line_id ? `· → ${esc(seg.line_id)}` : " · 未匹配"} · 相似 ${(seg.similarity * 100).toFixed(0)}%</div>
        <div class="spoken">说：${esc(seg.spoken_text)}</div>
        ${seg.script_text ? `<div class="script">本：${esc(seg.script_text)}</div>` : ""}
        ${seg.review_reasons?.length ? `<div class="script" style="color:var(--amber)">${esc(seg.review_reasons.join("; "))}</div>` : ""}`;
      const row = document.createElement("div");
      row.className = "seg-acts";
      const mk = (t: string, fn: () => void) => { const b = document.createElement("button"); b.textContent = t; b.onclick = fn; row.appendChild(b); };
      mk("试听", () => {
        if (s.audio?.take && player.mediaBase) {
          const a = new Audio(player.mediaURL(s.audio.take));
          a.currentTime = seg.start_ms / 1000;
          void a.play(); setTimeout(() => a.pause(), seg.end_ms - seg.start_ms + 200);
        }
      });
      if (seg.review_required && !seg.reviewed) {
        mk("确认无误", () => { seg.reviewed = true; seg.review_required = false; saveTimeline(); });
        mk("弃用", () => { seg.discarded = true; seg.selected = false; saveTimeline(); });
      }
      if (!seg.selected && seg.line_id) mk("选这条", () => {
        state.timeline!.segments.forEach((x) => { if (x.line_id === seg.line_id) x.selected = x === seg; });
        saveTimeline();
      });
      d.appendChild(row);
      tbl.appendChild(d);
    }
    const head = document.createElement("div");
    head.className = "row small";
    head.textContent = `对齐：${state.timeline.segments.length} 段 · 待审核 ${state.timeline.review_count} · 缺词 ${state.timeline.unmatched_lines?.length ?? 0}`;
    tbl.prepend(head);
  }
}

async function saveTimeline() {
  if (!state.scene?.audio?.timeline || !state.timeline) return;
  state.timeline.review_count = state.timeline.segments.filter((x) => x.review_required && !x.reviewed).length;
  try {
    await bridge.saveTimelineFile(state.scene.audio.timeline, JSON.stringify(state.timeline));
    toast("timeline 已存");
  } catch (e) {
    toast("timeline 保存失败: " + e);
    return;
  }
  // timeline 文件已变 → rendered 指纹对不上 → 重载 scene 拿新
  // audio_status；播放器检测到 stale 位翻转会失效缓存并停止旧配音。
  if (state.sceneDir) {
    applyScene(await bridge.loadScene(state.sceneDir), true);
  }
  renderVoice();
}

async function wireVoice() {
  state.presets = await bridge.presetList().catch(() => []);
  const ms = await bridge.modelStatus().catch(() => null);
  $("model-state").textContent = ms ? `ASR 模型：${ms.state}${ms.detail ? " — " + ms.detail : ""}` : "";
  $("btn-rec").onclick = async () => {
    if (!isReal) { toast("录音仅在 MyGo 应用内可用"); return; }
    if (rec.recording) {
      const b64 = await rec.stop();
      $("rec-state").textContent = "";
      $("rec-state").classList.remove("on");
      $("btn-rec").innerHTML = micIcon + " 录音";
      const name = `take-${new Date().toISOString().slice(11, 19).replaceAll(":", "")}`;
      const r = await bridge.saveRecording(state.ws, b64, name).catch((e) => { toast(String(e)); return null; });
      if (r?.path) {
        await edit({ op: "set_take", path: r.path });
        toast("录音已存 " + r.path.split("/").pop());
      }
    } else {
      try { await rec.start(); } catch (e) { toast("麦克风不可用: " + e); return; }
      $("rec-state").textContent = "录音中…";
      $("rec-state").classList.add("on");
      $("btn-rec").innerHTML = stopIcon + " 停止";
    }
  };
  $("btn-import-take").onclick = async () => {
    const f = await bridge.pickFile("选择录音文件", "wav");
    if (!f?.path) return;
    const r = await bridge.readFileB64(f.path).catch((e) => { toast(String(e)); return null; });
    if (!r) return;
    const name = f.path.split("/").pop()?.replace(/\.wav$/i, "") ?? "take";
    const saved = await bridge.saveRecording(state.ws, r.b64, name).catch((e) => { toast(String(e)); return null; });
    if (saved?.path) {
      await edit({ op: "set_take", path: saved.path });
      toast("已导入 take：" + saved.path.split("/").pop());
    }
  };
  $("btn-analyze").onclick = async () => {
    if (!state.scene?.audio?.take) return;
    setSave("ASR 对齐中…（无 GPU，约几秒-几十秒）");
    try {
      const r = await bridge.analyze(state.sceneDir, state.scene.audio.take) as any;
      if (r.promoted) {
        state.timeline = await bridge.loadTimeline(r.timeline);
      } else {
        // 输入变了或另一任务先完成——结果留作历史记录，不覆盖当前生效的对齐。
        toast("对齐结果未采用（已有更新版本生效）");
      }
      // 服务端已写好 take/timeline 指纹——以落盘的 scene 为准刷新 UI。
      applyScene(await bridge.loadScene(state.sceneDir), true);
      player.timeline = state.timeline;
      if (r.promoted) toast(`对齐完成：${r.segments} 段，待审 ${r.review_count}`);
    } catch (e) { toast("ASR 失败: " + e); }
    setSave("");
    refreshAll();
  };
  $("btn-render").onclick = async () => {
    if (!state.scene?.audio?.timeline) return;
    setSave("离线渲染配音…");
    try {
      const r = await bridge.renderScene(state.sceneDir) as any;
      // rendered 目录可能复用路径、内容已变——丢缓存重新读 manifest。
      player.invalidateAudio();
      applyScene(await bridge.loadScene(state.sceneDir), true);
      if (!r.promoted) {
        toast("渲染结果未采用（已有更新版本生效）");
      } else {
        toast(`渲染完成：${r.lines} 条 · ${r.status}`);
      }
    } catch (e) { toast("渲染失败: " + e); }
    setSave("");
    renderVoice();
  };
}

// ---------- 批注面板 ----------

function renderFeedback() {
  const s = state.scene;
  const box = $("fb-list");
  box.innerHTML = "";
  if (!s) return;
  const fbs = [...(s.feedback ?? [])].sort((a, b) => b.created.localeCompare(a.created));
  for (const f of fbs) {
    const d = document.createElement("div");
    d.className = `fb ${f.status}`;
    const l = s.script.lines.find((x) => x.line_id === f.line_id);
    // 批注引用悬空要可见：clip/line 被删或重生成换内容后，不能静默当成还有效。
    const clipGone = f.clip_id && !s.clips.find((x) => x.clip_id === f.clip_id);
    const lineGone = f.line_id && !l;
    const goneBadge = (clipGone || lineGone) ? ` <span class="badge err">目标已变更</span>` : "";
    d.innerHTML = `<div class="h"><span>${esc(f.id)} · ${esc(f.kind)} · ${(f.scene_ms / 1000).toFixed(1)}s${f.clip_id ? " · " + esc(f.clip_id) : ""}${goneBadge}</span></div>
      <div>${esc(f.text)}${f.voice ? " <span class='badge plain'>语音</span>" : ""}</div>
      ${l ? `<div class="line-ref">→ ${esc(l.line_id)}：${esc(l.text.slice(0, 20))}</div>` : ""}`;
    const foot = document.createElement("div");
    foot.style.cssText = "display:flex;align-items:center;margin-top:6px";
    const jump = document.createElement("button");
    jump.textContent = "定位"; jump.className = "btn"; jump.style.cssText = "font-size:11px;padding:1px 7px;height:20px";
    jump.onclick = () => void player.seek(f.scene_ms);
    const sel = document.createElement("select");
    for (const st of ["open", "doing", "done", "wontfix"]) {
      const o = document.createElement("option"); o.value = st;
      o.textContent = { open: "待处理", doing: "处理中", done: "已完成", wontfix: "不改" }[st]!;
      if (st === f.status) o.selected = true;
      sel.appendChild(o);
    }
    sel.onchange = () => edit({ op: "feedback_status", id: f.id, status: sel.value });
    foot.append(jump, sel);
    d.appendChild(foot);
    box.appendChild(d);
  }
  if (!fbs.length) box.innerHTML = `<div class="row small">在播放位按 ✎ 添加批注，自动带上 clip/台词/时间戳。</div>`;
}

function addFeedback(voicePath?: string) {
  const s = state.scene;
  if (!s) return;
  const at = player.clipAt(player.playhead);
  const cue = player.lineCue();
  // 找当前时间最近的 line
  let curLine = "";
  let best = 1e9;
  for (const [lid, ms] of cue) {
    if (ms <= player.playhead && player.playhead - ms < best) { best = player.playhead - ms; curLine = lid; }
  }
  edit({
    op: "add_feedback",
    scene_ms: Math.round(player.playhead),
    clip_id: at?.clip.clip_id ?? "",
    clip_ms: Math.round(at?.local ?? 0),
    line_id: curLine,
    kind: ($("fb-kind") as HTMLInputElement).value || "other",
    text: ($("fb-text") as HTMLTextAreaElement).value,
    voice: voicePath ?? "",
  }).then((ok) => {
    if (!ok) return; // 保存失败保留输入，不能清掉导演写好的批注
    ($("fb-text") as HTMLTextAreaElement).value = "";
    document.querySelector<HTMLButtonElement>(".segs .opt[data-tab=feedback]")?.click();
    toast("批注已记录");
  });
}

// ---------- 全局装配 ----------

function refreshAll() {
  renderScript(); renderTimeline(); renderProps(); renderFeedback(); renderVoice();
  $("btn-export-fb").toggleAttribute("disabled", !state.scene);
  $("btn-undo").toggleAttribute("disabled", !state.sceneDir);
  $("btn-redo").toggleAttribute("disabled", !state.sceneDir);
  const s = state.scene;
  if (s) {
    const nFin = s.clips.filter((c) => (c.candidates?.[c.pick ?? 0]?.kind) === "video").length;
    $("view-mode").textContent = `${s.clips.length} clips · ${nFin} 成片 · ${s.feedback?.filter((f) => f.status === "open").length ?? 0} 待处理批注`;
  }
}

async function boot() {
  // tabs（Magpie 分段控件）
  document.querySelectorAll<HTMLButtonElement>(".segs .opt").forEach((t) => {
    t.onclick = () => {
      document.querySelectorAll(".segs .opt").forEach((x) => x.classList.remove("on"));
      document.querySelectorAll(".tabpage").forEach((x) => x.classList.add("hidden"));
      t.classList.add("on");
      $(`tab-${t.dataset.tab}`).classList.remove("hidden");
    };
  });

  $("btn-workspace").onclick = async () => {
    const d = await bridge.pickDirectory("打开工作目录（示例：examples/animatic-demo/workbench）");
    if (!d?.path) return;
    state.ws = d.path;
    $("ws-label").textContent = d.path;
    const r = await bridge.openWorkspace(d.path);
    const sel = $("scene-select") as HTMLSelectElement;
    sel.innerHTML = "";
    for (const sc of r.scenes) {
      const o = document.createElement("option");
      o.value = sc.dir; o.textContent = `${sc.title} (${sc.scene_id})`;
      sel.appendChild(o);
    }
    sel.disabled = false;
    ($("btn-newscene") as HTMLButtonElement).disabled = false;
    if (r.scenes[0]) { sel.value = r.scenes[0].dir; await loadScene(r.scenes[0].dir); }
  };
  $("scene-select").onchange = async (e) => loadScene((e.target as HTMLSelectElement).value);
  $("btn-newscene").onclick = async () => {
    const title = window.prompt("场景标题", "新场景");
    if (!title || !state.ws) return;
    const r = await bridge.newScene(state.ws, title);
    const sel = $("scene-select") as HTMLSelectElement;
    const o = document.createElement("option");
    o.value = r.dir; o.textContent = `${r.scene.title} (${r.scene.scene_id})`;
    sel.appendChild(o); sel.value = r.dir;
    await loadScene(r.dir);
  };
  $("btn-gendraft").onclick = async () => {
    if (!state.sceneDir) return toast("先打开场景");
    const r = await bridge.genDraft(state.sceneDir) as any;
    applyScene(r.scene, true);
    refreshAll();
    toast(`生成 ${r.scene.clips.length} 个 draft clips`);
  };
  $("btn-import-script").onclick = async () => {
    const f = await bridge.pickFile("导入 script.json", "json");
    if (!f?.path || !state.scene) return;
    const t = await bridge.loadTextFile(f.path);
    const parsed = JSON.parse(t.text);
    state.scene.script = parsed;
    const saved = await bridge.saveScene(state.sceneDir, JSON.stringify(state.scene)).catch((e) => { toast(String(e)); return null; });
    if (saved) applyScene(saved, true);
    refreshAll();
    toast("台本已导入——点「生成预演」重建 clips");
  };
  $("btn-addline").onclick = async () => {
    if (!state.scene) return;
    const text = window.prompt("台词内容");
    if (!text) return;
    const role = state.scene.roles[0]?.id ?? "a";
    await edit({ op: "add_line", after_id: state.scene.script.lines.at(-1)?.line_id ?? "", role_id: role, text });
  };

  // transport
  $("btn-play").onclick = () => { if (player.playing) player.pause(); else void player.play(); };
  $("scrub").oninput = (e) => {
    const p = (e.target as HTMLInputElement).valueAsNumber / 1000;
    void player.seek(p * player.total());
  };
  $("btn-mark").onclick = () => addFeedback();
  $("btn-fb-add").onclick = () => addFeedback();
  $("btn-fb-voice").onclick = async () => {
    if (!isReal || !rec.recording) {
      if (!isReal) { toast("语音批注仅在 MyGo 应用内可用"); return; }
      try { await rec.start(); } catch (e) { toast("麦克风不可用: " + e); return; }
      ($("btn-fb-voice") as HTMLButtonElement).innerHTML = stopIcon;
      return;
    }
    const b64 = await rec.stop();
    ($("btn-fb-voice") as HTMLButtonElement).innerHTML = micIcon;
    const r = await bridge.saveRecording(state.ws, b64, `fb-${Date.now() % 100000}`);
    addFeedback(r.path);
  };
  $("btn-undo").onclick = async () => {
    const s = await bridge.undo(state.sceneDir).catch((e) => { toast(String(e)); return null; });
    if (s) { applyScene(s, true); refreshAll(); }
  };
  $("btn-redo").onclick = async () => {
    const s = await bridge.redo(state.sceneDir).catch((e) => { toast(String(e)); return null; });
    if (s) { applyScene(s, true); refreshAll(); }
  };
  $("btn-export-fb").onclick = async () => {
    const r = await bridge.exportFeedback(state.sceneDir).catch((e) => { toast(String(e)); return null; });
    if (r) toast(`已导出 ${r.md.split("/").pop()}`);
  };

  // player 回调
  player.onTick = (ms) => {
    $("tc").textContent = `${fmt(ms)} / ${fmt(player.total())}`;
    ($("scrub") as HTMLInputElement).value = String(Math.round(ms / Math.max(1, player.total()) * 1000));
    $("ic-play").innerHTML = player.playing ? pauseSVG : playSVG;
    const at = player.clipAt(ms);
    if (at) {
      const src = at.clip.candidates?.[at.clip.pick ?? 0];
      $("stage-hud").innerHTML = `${esc(at.clip.clip_id)} ${statusBadge(at.clip.status, at.clip.stale)} <span>${src?.kind === "video" ? "视频" : "draft"}</span>`;
    }
  };
  player.onClipChange = () => {};

  // 快捷键
  document.addEventListener("keydown", (e) => {
    if ((e.target as HTMLElement).matches("input,textarea")) return;
    if (e.code === "Space") { e.preventDefault(); if (player.playing) player.pause(); else void player.play(); }
    if (e.key === "m" || e.key === "M") addFeedback();
    if ((e.metaKey || e.ctrlKey) && e.key === "z" && !e.shiftKey) $("btn-undo").click();
    if ((e.metaKey || e.ctrlKey) && (e.key === "Z" || (e.key === "z" && e.shiftKey))) $("btn-redo").click();
  });

  await wireVoice();

  // mock 模式自动载示例场景，便于浏览器/playwright 直接查看
  if (!isReal) {
    const r = await bridge.openWorkspace("/mock/workspace");
    state.ws = r.dir;
    $("ws-label").textContent = r.dir + "（mock）";
    await loadScene("/mock/workspace/scenes/demo");
    toast("浏览器 mock 模式：编辑在内存生效，录音/ASR 不可用");
  }
  // 空闲时把 playhead 渲染一帧（scrub 后）
  setInterval(() => { if (!player.playing && state.scene) player.render(player.playhead); }, 120);
}

function esc(s: string) {
  const d = document.createElement("i"); d.textContent = s; return d.innerHTML;
}

void boot();
