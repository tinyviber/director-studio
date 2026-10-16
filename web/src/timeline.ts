// timeline.ts — Clips 时间线：拖动重排、点选、状态徽章、批注标记；
// 音频 lane 按台本行渲染块，点击试听该句配音。
import type { Scene, Clip } from "./bridge";

export interface TimelineEvents {
  onSelect: (clipID: string) => void;
  onReorder: (order: string[]) => void;
  onSeekClip: (clipID: string) => void;
  onPlayLine: (lineID: string) => void;
}

export function renderVideoLane(
  el: HTMLElement, scene: Scene, sel: string | null, playClipID: string | null,
  ev: TimelineEvents,
) {
  el.innerHTML = "";
  const fbByClip = new Map<string, number>();
  for (const f of scene.feedback ?? []) {
    if (f.clip_id && (f.status === "open" || f.status === "doing")) {
      fbByClip.set(f.clip_id, (fbByClip.get(f.clip_id) ?? 0) + 1);
    }
  }
  for (const id of scene.order) {
    const c = scene.clips.find((x) => x.clip_id === id);
    if (!c) continue;
    const d = document.createElement("div");
    d.className = "clip";
    d.draggable = true;
    if (c.clip_id === sel) d.classList.add("sel");
    if (c.stale) d.classList.add("stalef");
    const src = c.candidates?.[c.pick ?? 0];
    const st = c.status;
    const stBadge = st === "draft" ? `<span class="badge warn">草稿</span>`
      : st === "final" ? `<span class="badge ok">成片</span>`
      : `<span class="badge ok">已批准</span>`;
    const srcIcon = src?.kind === "video"
      ? `<svg viewBox="0 0 16 16" width="10" height="10" style="vertical-align:-1px"><rect x="1.5" y="3.5" width="9" height="9" rx="1.5" fill="none" stroke="currentColor" stroke-width="1.3"/><path d="M10.5 7.2l4-2.2v6l-4-2.2" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/></svg>`
      : `<svg viewBox="0 0 16 16" width="10" height="10" style="vertical-align:-1px"><path d="M10.5 3.5l2 2L6 12H4v-2l6.5-6.5z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/></svg>`;
    d.innerHTML = `
      <span class="t">${esc(c.clip_id)} ${esc(c.title ?? "")}</span>
      <span class="m">${stBadge}<span>${(c.dur_ms / 1000).toFixed(1)}s</span>
        <span>${c.line_ids?.length ?? 0}句</span>
        <span>${srcIcon}</span></span>`;
    const fb = fbByClip.get(c.clip_id);
    if (fb) {
      const m = document.createElement("span");
      m.className = "fbmark"; m.textContent = String(fb);
      m.title = `${fb} 条待处理批注`;
      d.appendChild(m);
    }
    d.addEventListener("click", () => { ev.onSelect(c.clip_id); ev.onSeekClip(c.clip_id); });
    d.addEventListener("dblclick", () => ev.onSeekClip(c.clip_id));
    // HTML5 DnD 重排
    d.addEventListener("dragstart", (e) => {
      e.dataTransfer!.setData("text/clip", c.clip_id);
      e.dataTransfer!.effectAllowed = "move";
    });
    d.addEventListener("dragover", (e) => {
      e.preventDefault();
      d.classList.add("drop-into");
    });
    d.addEventListener("dragleave", () => d.classList.remove("drop-into"));
    d.addEventListener("drop", (e) => {
      e.preventDefault();
      d.classList.remove("drop-into");
      const from = e.dataTransfer!.getData("text/clip");
      if (!from || from === c.clip_id) return;
      const order = scene.order.filter((x) => x !== from);
      order.splice(order.indexOf(c.clip_id), 0, from);
      ev.onReorder(order);
    });
    el.appendChild(d);
  }
}

export function renderAudioLane(
  el: HTMLElement, scene: Scene, ev: TimelineEvents,
  lineWavs: Map<string, string>, // line_id → wav 路径（rendered 或 take）
) {
  el.innerHTML = "";
  for (const id of scene.order) {
    const c = scene.clips.find((x) => x.clip_id === id);
    if (!c) continue;
    for (const lid of c.line_ids ?? []) {
      const l = scene.script.lines.find((x) => x.line_id === lid);
      if (!l) continue;
      const role = scene.roles.find((r) => r.id === l.role_id);
      const b = document.createElement("div");
      const has = lineWavs.get(lid);
      b.className = "ablk" + (has ? "" : " noaudio");
      b.innerHTML = `<div class="who">${esc(role?.name ?? l.role_id)}${has ? "" : " · 无音"}</div>
        <div>${esc(l.text.slice(0, 10))}${l.text.length > 10 ? "…" : ""}</div>`;
      b.title = `${lid} · ${l.text}`;
      b.addEventListener("click", () => ev.onPlayLine(lid));
      el.appendChild(b);
    }
  }
}

function esc(s: string) {
  const d = document.createElement("i");
  d.textContent = s;
  return d.innerHTML;
}
