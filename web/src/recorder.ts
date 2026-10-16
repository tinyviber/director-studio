// recorder.ts — webview 内录音：AudioWorklet 收 48k PCM，拼成 WAV16 base64
// 交给 Go 端落盘（SaveRecording）。与 voice-morph 的 worklet 思路一致，
// 但只收不回放（回放走 <audio>/AudioBufferSource）。
export class Recorder {
  private ctx: AudioContext | null = null;
  private node: AudioWorkletNode | null = null;
  private stream: MediaStream | null = null;
  private chunks: Float32Array[] = [];
  private n = 0;
  recording = false;

  async start(): Promise<void> {
    if (this.recording) return;
    this.stream = await navigator.mediaDevices.getUserMedia({
      audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true },
    });
    this.ctx = new AudioContext({ sampleRate: 48000 });
    const src = this.ctx.createMediaStreamSource(this.stream);
    // 内联 worklet 源码，避免 public 路径依赖打包布局
    const workletSrc = `
      class Rec extends AudioWorkletProcessor {
        constructor(){ super(); this.q=[]; }
        process(inputs){ const ch=inputs[0]&&inputs[0][0];
          if(ch){ this.q.push(...ch);
            while(this.q.length>=2048){
              const b=Float32Array.from(this.q.slice(0,2048));
              this.q=this.q.slice(2048);
              this.port.postMessage(b,[b.buffer]);
            } }
          return true; } }
      registerProcessor('rec', Rec);`;
    const url = URL.createObjectURL(new Blob([workletSrc], { type: "application/javascript" }));
    await this.ctx.audioWorklet.addModule(url);
    this.node = new AudioWorkletNode(this.ctx, "rec");
    this.chunks = []; this.n = 0;
    this.node.port.onmessage = (e) => {
      this.chunks.push(e.data as Float32Array);
      this.n += (e.data as Float32Array).length;
    };
    src.connect(this.node);
    // 不落回扬声器避免啸叫：worklet 输出丢到零增益
    const mute = this.ctx.createGain();
    mute.gain.value = 0;
    this.node.connect(mute).connect(this.ctx.destination);
    this.recording = true;
  }

  async stop(): Promise<string> {
    if (!this.recording) return "";
    this.recording = false;
    this.node?.disconnect();
    this.stream?.getTracks().forEach((t) => t.stop());
    await this.ctx?.close();
    const sr = this.ctx?.sampleRate ?? 48000;
    const pcm = new Float32Array(this.n);
    let o = 0;
    for (const c of this.chunks) { pcm.set(c, o); o += c.length; }
    return wavB64(pcm, sr);
  }
}

// Float32 单声道 → 16-bit PCM WAV base64
export function wavB64(pcm: Float32Array, sr: number): string {
  const n = pcm.length;
  const buf = new ArrayBuffer(44 + n * 2);
  const v = new DataView(buf);
  const wstr = (off: number, s: string) => { for (let i = 0; i < s.length; i++) v.setUint8(off + i, s.charCodeAt(i)); };
  wstr(0, "RIFF"); v.setUint32(4, 36 + n * 2, true); wstr(8, "WAVE");
  wstr(12, "fmt "); v.setUint32(16, 16, true); v.setUint16(20, 1, true);
  v.setUint16(22, 1, true); v.setUint32(24, sr, true); v.setUint32(28, sr * 2, true);
  v.setUint16(32, 2, true); v.setUint16(34, 16, true);
  wstr(36, "data"); v.setUint32(40, n * 2, true);
  for (let i = 0; i < n; i++) {
    let s = Math.max(-1, Math.min(1, pcm[i]!));
    v.setInt16(44 + i * 2, s < 0 ? s * 0x8000 : s * 0x7fff, true);
  }
  const u8 = new Uint8Array(buf);
  let bin = "";
  for (let i = 0; i < u8.length; i += 0x8000) bin += String.fromCharCode(...u8.subarray(i, i + 0x8000));
  return btoa(bin);
}
