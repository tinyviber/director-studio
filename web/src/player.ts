// player.ts — 连续播放引擎：draft clip 画 canvas，video clip 播 <video>，
// 台词音频按 beat 时间调度（rendered lines 优先，其次 src take 切片）。
// 核心是"真实时钟驱动的连续预览"——不是逐帧 GIF，而是可暂停/拖进度/
// 跨 clip 连播的时间轴。
import { Actors, drawActor, drawStage } from "./stickman";
import type { Clip, Scene, Timeline } from "./bridge";

// LineAudio 是一句台词当前应放的音频窗口：buf + [offMs, offMs+durMs)。
// rendered wav 是整段（offMs=0）；take 切片是源 wav 内的一段。
interface LineAudio { buf: AudioBuffer; offMs: number; durMs: number }

export class Player {
  cv: HTMLCanvasElement; vid: HTMLVideoElement; ctx2d: CanvasRenderingContext2D;
  actx: AudioContext | null = null;
  scene: Scene | null = null;
  timeline: Timeline | null = null;      // 对齐后的配音 timeline（取 src 切片）
  mediaBase = "";
  mediaKey = "";                          // 媒体服务会话令牌（MediaBase 下发）
  playing = false;
  playhead = 0;                           // scene 时间轴 ms
  starts: number[] = [];                  // 每个 ordered clip 的起点
  private raf = 0;
  private lastT = 0;
  private playGen = 0;                     // 播放请求代次：pause/新 play 使在途 play 失效
  private audioBufs = new Map<string, AudioBuffer | null>(); // wav 路径 → buffer
  private wavInflight = new Map<string, Promise<AudioBuffer | null>>();
  private manifestInflight: Promise<void> | null = null;
  private audioGen = 0;                            // 音频缓存代次：换场景/重渲染后丢弃在途结果
  private sources: AudioBufferSourceNode[] = [];
  private lineWav = new Map<string, string>();   // line_id → rendered wav 绝对路径
  private manifestDir = "";                       // 已加载 manifest 的 rendered 目录
  private schedGen = 0;                           // 调度代次：pause/seek 后丢弃在途结果
  actors = new Actors();
  private curClipIdx = -1;
  onTick: (ms: number, clipIdx: number) => void = () => {};
  onClipChange: (idx: number) => void = () => {};

  constructor(cv: HTMLCanvasElement, vid: HTMLVideoElement) {
    this.cv = cv; this.vid = vid;
    this.ctx2d = cv.getContext("2d")!;
    this.vid.addEventListener("ended", () => this.tick(performance.now()));
  }

  // mediaURL 拼带会话令牌的媒体地址；mediaBase 为空（无服务/mock）时不可用。
  mediaURL(path: string): string {
    return `${this.mediaBase}?p=${encodeURIComponent(path)}&k=${this.mediaKey}`;
  }

  // schedKey 决定已调度的台词音频是否仍然有效：只含影响播放编排的字段
  // （编排顺序、clip 源、line beats）——批注/备注/标题变化不该打断配音。
  private schedKeyFor(s: Scene | null): string {
    if (!s) return "";
    const parts: string[] = [];
    for (const id of s.order) {
      const c = s.clips.find((x) => x.clip_id === id);
      if (!c) continue;
      parts.push(`${id}:${c.candidates?.[c.pick ?? 0]?.kind ?? "draft"}:${c.dur_ms}`);
      for (const b of c.beats) {
        if (b.kind === "line") parts.push(`${b.line_id}@${b.at_ms}+${b.dur_ms ?? 0}`);
      }
    }
    return parts.join("|");
  }

  // audioKey 决定解码缓存/manifest 索引与已调度音频是否仍然有效：
  // 产物路径/指纹变了，或 audio_status 的 stale 位翻转（改台词、换 take、
  // 改音色、手动存了 timeline），旧音频都不该继续播。
  private audioKeyFor(s: Scene | null): string {
    const a = s?.audio;
    const st = (s?.meta?.audio_status ?? {}) as { timeline_stale?: boolean; rendered_stale?: boolean };
    if (!a) return st.timeline_stale || st.rendered_stale ? "stale" : "";
    return [a.take, a.take_sig, a.timeline, a.timeline_sig, a.rendered, a.rendered_sig,
      String(!!st.timeline_stale), String(!!st.rendered_stale)].join("|");
  }

  // setScene 换场景文档。preserve=true（编辑/undo 等同场景更新）时保留
  // 播放头；并区分两类更新——只动批注/元数据的更新不打断正在播的音频，
  // 动了编排或配音产物的更新才重调度并失效缓存。
  // false（换场景）时全部重置，避免上个场景的对齐数据污染新场景。
  setScene(s: Scene | null, preserve = false) {
    const prevHead = preserve ? this.playhead : 0;
    const schedChanged = this.schedKeyFor(this.scene) !== this.schedKeyFor(s);
    const audioChanged = this.audioKeyFor(this.scene) !== this.audioKeyFor(s);
    const rewire = schedChanged || audioChanged;
    if (!preserve || rewire) this.stopAudio();
    if (!preserve || audioChanged) this.invalidateAudio();
    this.scene = s;
    if (!preserve) this.timeline = null;
    this.starts = [];
    let t = 0;
    if (s) for (const id of s.order) {
      this.starts.push(t);
      const c = s.clips.find((x) => x.clip_id === id);
      t += c ? c.dur_ms : 0;
    }
    this.playhead = Math.max(0, Math.min(prevHead, this.total()));
    this.curClipIdx = -1;
    this.actors.reset();
    // 编排/配音变了但还在播：以当前播放头重建调度——画面不停、
    // 音频按新编排补回来，而不是画面继续走、声音却没了。
    if (preserve && rewire && this.playing) void this.scheduleFrom(this.playhead);
  }

  // invalidateAudio 在 take/timeline/rendered 变化后调用：丢弃解码缓存与
  // manifest 索引，确保下次播放拿到的是新产物。代次递增使在途的
  // fetch/decode 结果落地时直接被丢弃，不会回填旧场景/旧渲染的数据。
  invalidateAudio() {
    this.audioGen++;
    this.audioBufs.clear();
    this.wavInflight.clear();
    this.manifestInflight = null;
    this.lineWav.clear();
    this.manifestDir = "";
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
  // 只有成功才记 manifestDir——403/404/网络错误都允许下次重试；
  // 并发调用共享同一在途请求，不会重复拉。
  async loadManifest() {
    const dir = this.scene?.audio?.rendered;
    if (!dir || dir === this.manifestDir || !this.mediaBase) return;
    if (!this.manifestInflight) {
      const gen = this.audioGen;
      this.manifestInflight = (async () => {
        try {
          const r = await fetch(this.mediaURL(dir + "/manifest.json"));
          if (!r.ok || gen !== this.audioGen) return;
          const m = await r.json() as { lines?: { line_id?: string; wav?: string }[] };
          for (const ln of m.lines ?? []) {
            if (ln.line_id && ln.wav) this.lineWav.set(ln.line_id, `${dir}/${ln.wav}`);
          }
          this.manifestDir = dir;
        } catch { /* manifest 不在或网络断，下个触发点重试 */ }
      })().finally(() => { this.manifestInflight = null; });
    }
    return this.manifestInflight;
  }

  // renderedWav 返回某句当前索引到的 rendered wav 路径（给 UI 徽标用）。
  renderedWav(lineID: string): string | undefined { return this.lineWav.get(lineID); }

  private decodeWav(path: string): Promise<AudioBuffer | null> {
    const key = `wav:${path}`;
    if (this.audioBufs.has(key)) return Promise.resolve(this.audioBufs.get(key) ?? null);
    let p = this.wavInflight.get(key);
    if (!p) {
      const gen = this.audioGen;
      p = (async () => {
        try {
          const r = await fetch(this.mediaURL(path));
          if (!r.ok || !this.actx) return null;
          return await this.actx.decodeAudioData(await r.arrayBuffer());
        } catch { return null; }
      })().finally(() => { this.wavInflight.delete(key); });
      this.wavInflight.set(key, p);
      void p.then((b) => { if (gen === this.audioGen) this.audioBufs.set(key, b); });
    }
    return p;
  }

  // audioForLine 返回一句台词当前应放的音频窗口：rendered manifest 里选中的
  // wav 优先，其次 timeline 里对应 segment 的 take 切片（源 wav + [start,end]）。
  // 过期产物不进预览：stale 由服务端按指纹链算出——只显示徽标不拦播放
  // 等于拿旧音轨冒充新版本，反而比静音更有害。
  async audioForLine(lineID: string): Promise<LineAudio | null> {
    if (!this.scene || !this.actx) return null;
    const stale = (this.scene.meta?.audio_status ?? {}) as
      { timeline_stale?: boolean; rendered_stale?: boolean };
    // 1) rendered lines wav（manifest 映射）
    if (this.scene.audio?.rendered && this.mediaBase && !stale.rendered_stale) {
      await this.loadManifest();
      const p = this.lineWav.get(lineID);
      if (p) {
        const b = await this.decodeWav(p);
        if (b) return { buf: b, offMs: 0, durMs: b.duration * 1000 };
      }
    }
    // 2) take 切片（整条 take wav + 段内 [start,end] 窗口）
    if (this.scene.audio?.take && this.timeline && this.mediaBase && !stale.timeline_stale) {
      const seg = this.timeline.segments.find((s) => s.line_id === lineID && s.selected);
      if (seg) {
        const buf = await this.decodeWav(this.scene.audio.take);
        if (buf) return { buf, offMs: seg.start_ms, durMs: Math.max(0, seg.end_ms - seg.start_ms) };
      }
    }
    return null;
  }

  stopAudio() {
    this.schedGen++; // 在途 scheduleFrom 的 await 结果会被丢弃
    for (const s of this.sources) { try { s.stop(); } catch {} }
    this.sources = [];
  }

  // playLineOnce 试听单句（rendered 优先，否则 take 切片）。
  async playLineOnce(lineID: string): Promise<boolean> {
    this.ensureAudio();
    const au = await this.audioForLine(lineID);
    if (!au || !this.actx) return false;
    const src = this.actx.createBufferSource();
    src.buffer = au.buf;
    src.connect(this.actx.destination);
    // 用 durMs 判断（不是 offMs）——take 切片起点为 0ms 时也应只放该句窗口，
    // 否则会把整条 take 从头播完。
    const dur = au.durMs > 0
      ? Math.min(au.durMs / 1000 + 0.2, au.buf.duration - au.offMs / 1000)
      : undefined;
    src.start(0, au.offMs / 1000, dur);
    return true;
  }

  async play() {
    if (!this.scene || this.playing) return;
    this.ensureAudio();
    this.playing = true;
    const gen = ++this.playGen;
    this.lastT = performance.now(); // 兜底：等待期的 stray tick 不至于 dt 爆掉
    await this.scheduleFrom(this.playhead);
    // 等待期间被 pause/seek/新一次 play 取代时，不得再启动 RAF——
    // 否则两个 tick 循环会同时推进播放头。
    if (gen !== this.playGen || !this.playing) return;
    // 装载与调度耗时不计入播放时间，否则首帧会跳过等待时长。
    this.lastT = performance.now();
    const loop = (t: number) => {
      if (!this.playing) return;
      this.tick(t);
      this.raf = requestAnimationFrame(loop);
    };
    this.raf = requestAnimationFrame(loop);
  }

  pause() {
    this.playGen++; // 使在途的 play() 落地即失效
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
    const gen = this.schedGen;
    const at = this.clipAt(ms);
    if (!at || !this.actx) return;
    // 收集当前 clip 及其后 clips 的 line beats（在 scene 时钟上）；
    // 已绑定正式视频的 clip 自带音轨，不再叠台词配音。
    type Job = { abs: number; dur: number; lineID: string };
    const jobs: Job[] = [];
    for (let i = at.idx; i < this.scene!.order.length; i++) {
      const c = this.scene!.clips.find((x) => x.clip_id === this.scene!.order[i])!;
      if (c.candidates?.[c.pick ?? 0]?.kind === "video") continue;
      for (const b of c.beats) {
        if (b.kind !== "line" || !b.line_id) continue;
        const abs = this.starts[i]! + b.at_ms;
        if (abs + (b.dur_ms ?? 0) <= ms) continue; // 这句已播完
        jobs.push({ abs, dur: b.dur_ms ?? 0, lineID: b.line_id });
      }
    }
    // 并行取/解码所有 wav，而不是逐句串行等待。
    const aus = await Promise.all(jobs.map((j) => this.audioForLine(j.lineID)));
    if (gen !== this.schedGen || !this.actx) return; // 等待期间被 pause/seek 打断
    const base = this.actx.currentTime + 0.05;
    // 解码期间播放头可能已前进（播放中重调度）——以当前位置校准，
    // 否则音频会整体晚出这段装载耗时。
    const now = this.playhead;
    jobs.forEach((j, k) => {
      const au = aus[k];
      if (!au || !this.actx) return;
      // seek/重调度落到句子中间时，从切片内对应偏移继续放。
      const lead = Math.max(0, now - j.abs);
      const offS = au.offMs / 1000 + lead / 1000;
      const avail = au.buf.duration - offS;
      const want = Math.min(j.dur > 0 ? (j.dur - lead) / 1000 + 0.3 : avail, avail);
      if (want <= 0) return;
      const src = this.actx.createBufferSource();
      src.buffer = au.buf;
      src.connect(this.actx.destination);
      src.start(base + Math.max(0, j.abs - now) / 1000, offS, want);
      this.sources.push(src);
    });
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

  private videoURL(clip: Clip): string | null {
    const src = clip.candidates?.[clip.pick ?? 0];
    if (src?.kind === "video" && src.path && this.mediaBase) {
      return this.mediaURL(src.path);
    }
    return null;
  }

  private onClipEntered(clip: Clip, localMs: number) {
    // video 源：把 <video> seek 到 t0+local 并播
    const url = this.videoURL(clip);
    if (url) {
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
    const url = this.videoURL(clip);
    const isVideo = !!url;
    this.vid.style.display = isVideo ? "block" : "none";
    this.cv.style.display = isVideo ? "none" : "block";
    if (isVideo) {
      // 暂停态 seek 进来也要切到正确 src 并对齐帧——src 之前只在
      // onClipEntered（播放中）设置。
      if (this.vid.src !== url) this.vid.src = url!;
      const want = ((clip.t0_ms ?? 0) + local) / 1000;
      if (this.vid.paused && Math.abs(this.vid.currentTime - want) > 0.04) {
        this.vid.currentTime = want;
      }
      return;
    }

    // ---- draft 渲染 ----
    const W = this.cv.width, H = this.cv.height;
    // 推进 actors 到 scene 时刻：先让之前所有 clip 的动作跑完（保持跨 clip
    // 的位置/朝向/姿态连续），再把当前 clip 播到 local。全量重放是确定性的。
    this.actors.reset();
    if (this.scene) {
      for (let i = 0; i < at.idx; i++) {
        const pc = this.scene.clips.find((x) => x.clip_id === this.scene!.order[i]);
        if (pc) this.actors.applyBeats(pc.beats, pc.dur_ms);
      }
    }
    this.actors.applyBeats(clip.beats, local);
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
    } else if (camName === "pull") {
      cam = { cx: 0.5, cy: 0.5, zoom: 1.8 - 0.8 * Math.min(1, local / clip.dur_ms) };
    } else if (camName === "black") {
      this.ctx2d.fillStyle = "#16181c"; this.ctx2d.fillRect(0, 0, W, H);
      this.drawCaptions(clip, local); return;
    }
    drawStage(this.ctx2d, W, H, cam);
    if (this.scene) {
      for (const r of this.scene.roles) {
        drawActor(this.ctx2d, this.actors.get(r.id), r.color || "#9fd0ff", W, H, cam, ms);
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
