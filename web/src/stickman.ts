// stickman.ts — 参数化火柴人：draft animatic 的最小视觉单元。
//
// 设计目标：保留空间关系、动作因果、时机与镜头节奏——不是画得好。
// 每个 actor 是一个带位置/朝向/姿态状态的火柴人；action 事件在
// [at, at+dur] 内把姿态参数插值过去（walk/enter 有位移，talk 有口型抖动）。
//
// 状态推进是纯函数式的：播放器每次渲染先 reset，再按顺序重放之前所有
// clip 的动作 beats（跨 clip 保持位置/朝向连续），最后播当前 clip 到
// local 时刻。同一 scene 时间点永远得到同一姿态，不靠 wall clock。
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

const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));

export class Actors {
  private map = new Map<string, ActorState>();

  get(id: string): ActorState {
    if (!this.map.has(id)) this.map.set(id, defaultState());
    return this.map.get(id)!;
  }

  reset() { this.map.clear(); }

  // applyBeats 把 clip 内的 action beats 推进到 t 时刻（clip 内时钟）。
  // 传入 t >= clip.dur_ms 表示该 clip 已播完，所有动作取完成态——
  // 这是跨 clip 连续性（位置、朝向）的来源。
  applyBeats(beats: { at_ms: number; kind: string; actor?: string; action?: string; dur_ms?: number; from?: number[]; to?: number[] }[], t: number) {
    for (const b of beats) {
      if (b.kind !== "action" || !b.actor) continue;
      const a = this.get(b.actor);
      const dur = b.dur_ms && b.dur_ms > 0 ? b.dur_ms : 800;
      const p = t <= b.at_ms ? 0 : Math.min(1, (t - b.at_ms) / dur);
      if (p === 0) continue;
      // 位移起点：beat 显式给了 from 用 from，否则用推进到此 beat
      // 之前的实际位置（保证跨 beat/跨 clip 连续，不跳回默认点）。
      const from: Vec2 = b.from && b.from.length >= 2
        ? { x: b.from[0]!, y: b.from[1]! }
        : { x: a.pos.x, y: a.pos.y };
      switch (b.action) {
        case "enter": {
          // enter 是从场外走到 to 的位移，不是瞬移；重新入场要恢复站立，
          // 否则上一次 exit 的 pose 会让人物保持不可见。
          if (b.to && b.to.length >= 2) {
            a.pose = "stand";
            a.pos = p < 1
              ? { x: from.x + (b.to[0]! - from.x) * p, y: from.y + (b.to[1]! - from.y) * p }
              : { x: b.to[0]!, y: b.to[1]! };
            a.walking = p < 1;
          }
          break;
        }
        case "exit": a.pose = "exit"; a.walking = false; break;
        case "walk": {
          // 没给目标时按朝向推一段确定距离——walk 必须真的改变位置。
          const dist = 0.09 * (dur / 1000);
          const to: Vec2 = b.to && b.to.length >= 2
            ? { x: b.to[0]!, y: b.to[1]! }
            : { x: clamp(from.x + a.facing * dist, 0.04, 0.96), y: from.y };
          a.pos = p < 1
            ? { x: from.x + (to.x - from.x) * p, y: from.y + (to.y - from.y) * p }
            : to;
          a.walking = p < 1;
          break;
        }
        case "sit": if (p > 0.3) a.pose = "sit"; break;
        case "stand": if (p > 0.3) a.pose = "stand"; break;
        case "point": if (p > 0.2 && p < 1) a.pose = "point"; else if (p >= 1) a.pose = "stand"; break;
        case "turn":
          // 过半程翻面；重放语义下每个完成的 turn 累计翻一次，朝向保留。
          if (p >= 0.5) a.facing = (a.facing * -1) as 1 | -1;
          break;
        case "surprise": if (p < 1) a.pose = "surprise"; break;
        case "smile": a.emotion = "smile"; break;
        case "sad": a.pose = "sad"; a.emotion = "sad"; break;
        case "nod": if (p < 1) a.emotion = "nod"; break;
        case "shake": if (p < 1) a.emotion = "shake"; break;
        case "idle": if (p < 1) a.pose = "idle"; break;
        case "talk": a.mouthOpen = p < 1 ? (Math.sin(t / 90) + 1) / 2 : 0; break;
      }
    }
  }
}

// ---------- 绘制 ----------

// tMs 是场景时间轴毫秒数——行走摆动按场景时间驱动，同一时间点姿态一致。
export function drawActor(
  ctx: CanvasRenderingContext2D, a: ActorState, color: string,
  W: number, H: number, cam: { cx: number; cy: number; zoom: number }, tMs: number,
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
  const swing = a.walking ? Math.sin(tMs / 120) * h * 0.10 : 0;
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
    const lswing = a.walking ? Math.sin(tMs / 120) * h * 0.12 : 0;
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
