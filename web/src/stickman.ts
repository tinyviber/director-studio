// stickman.ts — 参数化火柴人：draft animatic 的最小视觉单元。
//
// 设计目标：保留空间关系、动作因果、时机与镜头节奏——不是画得好。
// 每个 actor 是一个带位置/朝向/姿态状态的火柴人；action 事件在
// [at, at+dur] 内把姿态参数插值过去（walk 有位移，talk 有口型抖动）。
export interface Vec2 { x: number; y: number }

export interface ActorState {
  pos: Vec2;        // 归一化坐标 0..1
  facing: 1 | -1;   // 朝左/右
  pose: "stand" | "sit" | "point" | "surprise" | "sad" | "idle" | "exit";
  mouthOpen: number; // 0..1 说话口型
  walking: boolean;
  emotion: string;  // smile|sad|"" 面部情绪
}

const defaultState = (): ActorState => ({
  pos: { x: -0.2, y: 0.82 }, facing: 1, pose: "stand", mouthOpen: 0, walking: false, emotion: "",
});

export class Actors {
  private map = new Map<string, ActorState>();
  private prev = new Map<string, ActorState>();

  get(id: string): ActorState {
    if (!this.map.has(id)) this.map.set(id, defaultState());
    return this.map.get(id)!;
  }

  // applyBeat 在 clip 内 t 时刻前推进 actor 状态（单调时间播放假设：
  // seek 时先 reset 再从 0 重放到 t，保证轨迹正确）。
  reset() { this.map.clear(); this.prev.clear(); }

  applyBeats(beats: { at_ms: number; kind: string; actor?: string; action?: string; dur_ms?: number; to?: number[] }[], t: number) {
    for (const b of beats) {
      if (b.kind !== "action" || !b.actor) continue;
      const a = this.get(b.actor);
      const dur = b.dur_ms && b.dur_ms > 0 ? b.dur_ms : 800;
      const p = t < b.at_ms ? 0 : Math.min(1, (t - b.at_ms) / dur);
      switch (b.action) {
        case "enter": if (p > 0 && b.to) a.pos = { x: b.to[0]!, y: b.to[1]! }; break;
        case "exit": if (p > 0) a.pose = "exit"; break;
        case "walk": {
          if (b.to) {
            const from = this.prev.get(b.actor)?.pos ?? a.pos;
            if (p < 1) {
              a.walking = true;
              a.pos = { x: from.x + (b.to[0]! - from.x) * p, y: b.to[1]! };
            } else { a.pos = { x: b.to[0]!, y: b.to[1]! }; a.walking = false; }
          } else a.walking = p > 0 && p < 1;
          break;
        }
        case "sit": if (p > 0.3) a.pose = "sit"; break;
        case "stand": if (p > 0.3) a.pose = "stand"; break;
        case "point": if (p > 0.2 && p < 1) a.pose = "point"; else if (p >= 1) a.pose = "stand"; break;
        case "turn": if (p > 0 && p < 1) a.facing = (a.facing * -1) as 1 | -1; break;
        case "surprise": if (p > 0 && p < 1) a.pose = "surprise"; break;
        case "smile": if (p > 0) a.emotion = "smile"; break;
        case "sad": if (p > 0) { a.pose = "sad"; a.emotion = "sad"; } break;
        case "nod": if (p > 0 && p < 1) a.emotion = "nod"; break;
        case "shake": if (p > 0 && p < 1) a.emotion = "shake"; break;
        case "idle": if (p > 0 && p < 1) a.pose = "idle"; break;
        case "talk": a.mouthOpen = p > 0 && p < 1 ? (Math.sin(t / 90) + 1) / 2 : 0; break;
      }
    }
  }

  snapshot() { this.prev = new Map([...this.map].map(([k, v]) => [k, { ...v, pos: { ...v.pos } }])); }
}

// ---------- 绘制 ----------

export function drawActor(
  ctx: CanvasRenderingContext2D, a: ActorState, color: string,
  W: number, H: number, cam: { cx: number; cy: number; zoom: number },
) {
  if (a.pose === "exit") return;
  const scale = cam.zoom;
  const cx = (a.pos.x - cam.cx) * scale + 0.5;
  const cy = (a.pos.y - cam.cy) * scale + 0.5;
  const x = cx * W, y = cy * H;
  const h = 0.30 * H * scale; // 身高
  const headR = h * 0.16;

  ctx.save();
  ctx.translate(x, y);
  ctx.scale(a.facing, 1);
  ctx.strokeStyle = color;
  ctx.fillStyle = color;
  ctx.lineWidth = Math.max(3.5, h * 0.05);
  ctx.lineCap = "round";

  const sit = a.pose === "sit";
  const torsoTop = sit ? -h * 0.52 : -h * 0.62;
  const hipY = sit ? -h * 0.16 : -h * 0.34;

  // 头
  const headY = torsoTop - headR * 0.9;
  ctx.beginPath();
  ctx.arc(0, headY, headR, 0, Math.PI * 2);
  ctx.stroke();
  // 面部：眼 + 情绪嘴 + 口型
  ctx.fillStyle = color;
  ctx.beginPath(); ctx.arc(headR * 0.35, headY - headR * 0.15, h * 0.018 + 1, 0, Math.PI * 2); ctx.fill();
  const mo = a.mouthOpen;
  ctx.beginPath();
  if (a.emotion === "smile") ctx.arc(headR * 0.15, headY + headR * 0.2, headR * 0.35, 0.15 * Math.PI, 0.85 * Math.PI);
  else if (a.emotion === "sad") ctx.arc(headR * 0.15, headY + headR * 0.5, headR * 0.35, 1.15 * Math.PI, 1.85 * Math.PI);
  else if (a.emotion === "nod") ctx.ellipse(headR * 0.15, headY + headR * 0.3, headR * 0.2, headR * 0.1, 0, 0, Math.PI * 2);
  else if (a.emotion === "shake") { ctx.moveTo(-headR * 0.2, headY + headR * 0.3); ctx.lineTo(headR * 0.5, headY + headR * 0.3); }
  else ctx.ellipse(headR * 0.15, headY + headR * 0.3, headR * 0.15, headR * (0.06 + mo * 0.25), 0, 0, Math.PI * 2);
  ctx.stroke();

  // 惊讶标记
  if (a.pose === "surprise") {
    ctx.font = `${h * 0.3}px sans-serif`;
    ctx.fillText("!", headR * 1.4, headY - headR);
  }

  // 躯干
  ctx.beginPath(); ctx.moveTo(0, torsoTop); ctx.lineTo(0, hipY); ctx.stroke();

  // 手臂：point 时右臂前伸，否则自然下垂 + 行走摆动
  const swing = a.walking ? Math.sin(performance.now() / 120) * h * 0.10 : 0;
  const shoulderY = torsoTop + h * 0.06;
  const armLen = h * 0.28;
  if (a.pose === "point") {
    ctx.beginPath(); ctx.moveTo(0, shoulderY); ctx.lineTo(armLen * 1.25, shoulderY - armLen * 0.35); ctx.stroke();
  } else {
    ctx.beginPath(); ctx.moveTo(0, shoulderY); ctx.lineTo(armLen * 0.5 + swing, shoulderY + armLen * 0.85); ctx.stroke();
  }
  ctx.beginPath(); ctx.moveTo(0, shoulderY); ctx.lineTo(-armLen * 0.5 - swing, shoulderY + armLen * 0.85); ctx.stroke();

  // 腿
  const legLen = sit ? h * 0.16 : h * 0.34;
  if (sit) {
    ctx.beginPath(); ctx.moveTo(0, hipY); ctx.lineTo(legLen, hipY + legLen * 0.2); ctx.lineTo(legLen, 0); ctx.stroke();
    ctx.beginPath(); ctx.moveTo(0, hipY); ctx.lineTo(-legLen * 0.4, hipY + legLen * 0.2); ctx.lineTo(-legLen * 0.4, 0); ctx.stroke();
  } else {
    const lswing = a.walking ? Math.sin(performance.now() / 120) * h * 0.12 : 0;
    ctx.beginPath(); ctx.moveTo(0, hipY); ctx.lineTo(legLen * 0.4 + lswing, 0); ctx.stroke();
    ctx.beginPath(); ctx.moveTo(0, hipY); ctx.lineTo(-legLen * 0.4 - lswing, 0); ctx.stroke();
  }
  ctx.restore();
}

// 地面线 + 场景标签
export function drawStage(ctx: CanvasRenderingContext2D, W: number, H: number, cam: { zoom: number }) {
  ctx.fillStyle = "#1e2126";
  ctx.fillRect(0, 0, W, H);
  ctx.strokeStyle = "#3a4049";
  ctx.lineWidth = 2;
  const gy = 0.82 * H;
  ctx.beginPath(); ctx.moveTo(0, gy); ctx.lineTo(W, gy); ctx.stroke();
}
