// player.ts — 连续播放引擎：draft clip 画 canvas，video clip 播 <video>，
// 台词音频按 beat 时间调度（rendered lines 优先，其次 src take 切片）。
// 核心是"真实时钟驱动的连续预览"——不是逐帧 GIF，而是可暂停/拖进度/
// 跨 clip 连播的时间轴。
import { Actors, drawActor, drawStage } from "./stickman";
import type { Clip, Scene, Timeline } from "./bridge";
import { bridge } from "./bridge";

interface SchedSrc { buf: AudioBuffer; at: number; dur: number }

export class Player {
  cv: HTMLCanvasElement; vid: HTMLVideoElement; ctx2d: CanvasRenderingContext2D;
  actx: AudioContext | null = null;
  scene: Scene | null = null;
  timeline: Timeline | null = null;      // 对齐后的配音 timeline（取 src 切片）
  renderedDir: string = "";               // dubbing rendered/ 目录
  mediaBase = "";
  playing = false;
  playhead = 0;                           // scene 时间轴 ms
  starts: number[] = [];                  // 每个 ordered clip 的起点
  private raf = 0;
  private lastT = 0;
  private audioBufs = new Map<string, AudioBuffer | null>(); // path → buffer
  private sources: AudioBufferSourceNode[] = [];
  private lineWav = new Map<string, string>();   // line_id → rendered wav 绝对路径
  private manifestDir = "";                       // 已加载 manifest 的 rendered 目录
  actors = new Actors();
  private curClipIdx = -1;
  onTick: (ms: number, clipIdx: number) => void = () => {};
  onClipChange: (idx: number) => void = () => {};

  constructor(cv: HTMLCanvasElement, vid: HTMLVideoElement) {
    this.cv = cv; this.vid = vid;
    this.ctx2d = cv.getContext("2d")!;
    this.vid.addEventListener("ended", () => this.tick(performance.now()));
  }

  setScene(s: Scene | null) {
    this.scene = s;
    this.starts = [];
    let t = 0;
    if (s) for (const id of s.order) {
      this.starts.push(t);
      const c = s.clips.find((x) => x.clip_id === id);
      t += c ? c.dur_ms : 0;
    }
    this.playhead = 0;
    this.curClipIdx = -1;
    this.actors.reset();
  }

  total(): number { return this.scene ? this.scene.clips.reduce((a, c) => a + c.dur_ms, 0) : 0; }

  clipAt(ms: number): { clip: Clip; idx: number; local: number } | null {
    if (!this.scene) return null;
    let t = 0, idx = 0;
    for (const id of this.scene.order) {
      const c = this.scene.clips.find((x) => x.clip_id === id);
      if (!c) continue;
      if (ms < t + c.dur_ms || idx === this.scene.order.length - 1) {
        return { clip: c, idx, local: Math.max(0, ms - t) };
      }
      t += c.dur_ms; idx++;
    }
    return null;
  }

  private ensureAudio() {
    if (!this.actx) this.actx = new AudioContext();
    if (this.actx.state === "suspended") void this.actx.resume();
  }

  // 加载 rendered/manifest.json → line_id 到 wav 文件名的映射。
  private async loadManifest() {
    const dir = this.scene?.audio?.rendered;
    if (!dir || dir === this.manifestDir || !this.mediaBase) return;
    try {
      const r = await fetch(`${this.mediaBase}?p=${encodeURIComponent(dir + "/manifest.json")}`);
      if (r.ok) {
        const m = await r.json() as { lines?: { line_id?: string; wav?: string }[] };
        for (const ln of m.lines ?? []) {
          if (ln.line_id && ln.wav) this.lineWav.set(ln.line_id, `${dir}/${ln.wav}`);
        }
      }
    } catch { /* manifest 不在或网络断 */ }
    this.manifestDir = dir;
  }

  // lineWavFor 返回一句台词当前应放的音频：rendered manifest 里选中的 wav
  // 优先，其次 timeline 里对应 segment 的 take 切片（源 wav + [start,end]）。
  async audioForLine(lineID: string): Promise<{ buf: AudioBuffer; offMs: number } | null> {
    if (!this.scene || !this.actx) return null;
    // 1) rendered lines wav（manifest 映射）
    if (this.scene.audio?.rendered && this.mediaBase) {
      await this.loadManifest();
      const p = this.lineWav.get(lineID);
      if (p) {
        const key = `rendered:${lineID}`;
        if (!this.audioBufs.has(key)) {
          try {
            const r = await fetch(`${this.mediaBase}?p=${encodeURIComponent(p)}`);
            this.audioBufs.set(key, r.ok ? await this.actx.decodeAudioData(await r.arrayBuffer()) : null);
          } catch { this.audioBufs.set(key, null); }
        }
        const b = this.audioBufs.get(key);
        if (b) return { buf: b, offMs: 0 };
      }
    }
    // 2) take 切片（整条 take wav + 段内 offset）
    if (this.scene.audio?.take && this.timeline) {
      const seg = this.timeline.segments.find((s) => s.line_id === lineID && s.selected);
      if (seg) {
        const key = `take:${this.scene.audio.take}`;
        if (!this.audioBufs.has(key)) {
          try {
            const r = await fetch(`${this.mediaBase}?p=${encodeURIComponent(this.scene.audio.take)}`);
            if (r.ok) this.audioBufs.set(key, await this.actx.decodeAudioData(await r.arrayBuffer()));
          } catch { /* ignore */ }
        }
        const buf = this.audioBufs.get(key);
        if (buf) return { buf, offMs: seg.start_ms };
      }
    }
    return null;
  }

  stopAudio() {
    for (const s of this.sources) { try { s.stop(); } catch {} }
    this.sources = [];
  }

  // playLineOnce 试听单句（rendered 优先，否则 take 切片）。
  async playLineOnce(lineID: string): Promise<boolean> {
    this.ensureAudio();
    const au = await this.audioForLine(lineID);
    if (!au || !this.actx) return false;
    const seg = !au.offMs ? null : this.timeline?.segments.find((s) => s.line_id === lineID && s.selected);
    const src = this.actx.createBufferSource();
    src.buffer = au.buf;
    if (au.offMs) src.start(0, au.offMs / 1000, seg ? (seg.end_ms - seg.start_ms) / 1000 + 0.2 : undefined);
    else src.start(0);
    return true;
  }

  async play() {
    if (!this.scene || this.playing) return;
    this.ensureAudio();
    this.playing = true;
    this.lastT = performance.now();
    await this.scheduleFrom(this.playhead);
    const loop = (t: number) => {
      if (!this.playing) return;
      this.tick(t);
      this.raf = requestAnimationFrame(loop);
    };
    this.raf = requestAnimationFrame(loop);
  }

  pause() {
    this.playing = false;
    cancelAnimationFrame(this.raf);
    this.stopAudio();
    this.vid.pause();
  }

  async seek(ms: number) {
    const was = this.playing;
    this.pause();
    this.playhead = Math.max(0, Math.min(ms, this.total()));
    this.actors.reset();
    this.curClipIdx = -1;
    if (was) await this.play();
    else this.render(this.playhead);
  }

  private async scheduleFrom(ms: number) {
    this.stopAudio();
    const at = this.clipAt(ms);
    if (!at) return;
    // 为当前 clip 及其后 clips 的 line beats 排音频（在 scene 时钟上）
    for (let i = at.idx; i < this.scene!.order.length; i++) {
      const c = this.scene!.clips.find((x) => x.clip_id === this.scene!.order[i])!;
      for (const b of c.beats) {
        if (b.kind !== "line" || !b.line_id) continue;
        const abs = this.starts[i]! + b.at_ms;
        if (abs < ms) continue;
        const au = await this.audioForLine(b.line_id);
        if (au && this.actx) {
          const src = this.actx.createBufferSource();
          src.buffer = au.buf;
          const when = this.actx.currentTime + (abs - ms) / 1000;
          if (au.offMs) src.start(when, au.offMs / 1000, Math.min((b.dur_ms ?? 0) / 1000 + 1, au.buf.duration - au.offMs / 1000));
          else src.start(when, 0, au.buf.duration);
          this.sources.push(src);
        }
      }
    }
  }

  private tick(now: number) {
    if (!this.playing) return;
    const dt = now - this.lastT;
    this.lastT = now;
    this.playhead += dt;
    if (this.playhead >= this.total()) { this.pause(); this.playhead = this.total(); this.onTick(this.playhead, -1); return; }
    this.render(this.playhead);
    const at = this.clipAt(this.playhead);
    if (at && at.idx !== this.curClipIdx) { this.curClipIdx = at.idx; this.onClipChange(at.idx); this.onClipEntered(at.clip, at.local); }
    this.onTick(this.playhead, at?.idx ?? -1);
  }

  private onClipEntered(clip: Clip, localMs: number) {
    // video 源：把 <video> seek 到 t0+local 并播
    const src = clip.candidates?.[clip.pick ?? 0];
    if (src?.kind === "video" && src.path && this.mediaBase) {
      const url = `${this.mediaBase}?p=${encodeURIComponent(src.path)}`;
      if (this.vid.src !== url) this.vid.src = url;
      this.vid.currentTime = ((clip.t0_ms ?? 0) + localMs) / 1000;
      this.vid.muted = false;
      void this.vid.play().catch(() => {});
    } else this.vid.pause();
  }

  // render 画当前帧（draft → canvas stickman；video → 显示 <video>）。
  render(ms: number) {
    const at = this.clipAt(ms);
    if (!at) { this.ctx2d.clearRect(0, 0, this.cv.width, this.cv.height); return; }
    const { clip, local } = at;
    const src = clip.candidates?.[clip.pick ?? 0];
    const isVideo = src?.kind === "video" && src.path && this.mediaBase;
    this.vid.style.display = isVideo ? "block" : "none";
    this.cv.style.display = isVideo ? "none" : "block";
    if (isVideo) { if (this.vid.paused) { this.vid.currentTime = ((clip.t0_ms ?? 0) + local) / 1000; } return; }

    // ---- draft 渲染 ----
    const W = this.cv.width, H = this.cv.height;
    // 推进 actors 到 local 时刻（clip 内时钟）。reset+重放保证轨迹对。
    this.actors.reset();
    this.actors.applyBeats(clip.beats, local);
    this.actors.snapshot();
    // 镜头：最近一个 camera beat
    let cam = { cx: 0.5, cy: 0.5, zoom: 1 };
    let camName = "wide";
    for (const b of clip.beats) {
      if (b.kind === "camera" && b.at_ms <= local) camName = b.text || "wide";
    }
    if (camName === "med" || camName === "cu") {
      // 跟到正在说话/最近动作的 actor
      const focus = clip.beats.filter((b) => b.kind === "line" && b.at_ms <= local && local <= b.at_ms + (b.dur_ms ?? 0)).pop();
      const a = focus?.actor ? this.actors.get(focus.actor) : null;
      const z = camName === "cu" ? 2.6 : 1.7;
      cam = a ? { cx: a.pos.x, cy: a.pos.y - 0.1, zoom: z } : { cx: 0.5, cy: 0.45, zoom: z };
    } else if (camName === "push") {
      cam = { cx: 0.5, cy: 0.5, zoom: 1 + 0.8 * Math.min(1, local / clip.dur_ms) };
    } else if (camName === "black") {
      this.ctx2d.fillStyle = "#16181c"; this.ctx2d.fillRect(0, 0, W, H);
      this.drawCaptions(clip, local); return;
    }
    drawStage(this.ctx2d, W, H, cam);
    if (this.scene) {
      for (const r of this.scene.roles) {
        drawActor(this.ctx2d, this.actors.get(r.id), r.color || "#9fd0ff", W, H, cam);
      }
    }
    this.drawCaptions(clip, local);
  }

  private drawCaptions(clip: Clip, local: number) {
    if (!this.scene) return;
    const W = this.cv.width, H = this.cv.height;
    const ctx = this.ctx2d;
    const line = clip.beats.find((b) => b.kind === "line" && b.at_ms <= local && local < b.at_ms + (b.dur_ms ?? 800));
    if (line?.line_id) {
      const l = this.scene.script.lines.find((x) => x.line_id === line.line_id);
      const role = this.scene.roles.find((r) => r.id === line.actor);
      if (l) {
        ctx.font = `${Math.round(H * 0.045)}px "PingFang SC", sans-serif`;
        ctx.textAlign = "center";
        const txt = `${role?.name ?? line.actor}：${l.text}`;
        const y = H * 0.94;
        ctx.strokeStyle = "rgba(0,0,0,.8)"; ctx.lineWidth = 4;
        ctx.strokeText(txt, W / 2, y);
        ctx.fillStyle = "#f4f2ec"; ctx.fillText(txt, W / 2, y);
      }
    }
    // HUD：clip 名 + 镜头
    ctx.textAlign = "left";
    ctx.font = `${Math.round(H * 0.03)}px sans-serif`;
    ctx.fillStyle = "rgba(255,255,255,.55)";
    ctx.fillText(`${clip.clip_id}  ${clip.title ?? ""}`, 12, 24);
  }

  // lineCue: line_id → scene_ms（播放头定位用）
  lineCue(): Map<string, number> {
    const m = new Map<string, number>();
    if (!this.scene) return m;
    this.scene.order.forEach((id, i) => {
      const c = this.scene!.clips.find((x) => x.clip_id === id)!;
      for (const b of c.beats) if (b.kind === "line" && b.line_id) m.set(b.line_id, this.starts[i]! + b.at_ms);
    });
    return m;
  }
}
