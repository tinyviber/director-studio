import { defineConfig } from "mygo-cli";

export default defineConfig({
  name: "DirectorStudio",
  identifier: "dev.videostudio.director",
  version: "0.1.0",
  copyright: "© 2026 Director Studio",
  // `mygo dev` runs devCommand and loads devUrl; `mygo build` runs
  // buildCommand and embeds frontendDist into the app.
  devUrl: "http://localhost:5174",
  devCommand: "bun run dev:web",
  buildCommand: "bun run build:web",
  frontendDist: "dist",
  bindings: "web/src/mygo.ts",
  resources: ["asr"],
  out: "build",
  macos: {
    minimumSystemVersion: "13.0",
    infoPlist: {
      NSMicrophoneUsageDescription:
        "导演工作台需要访问麦克风来录制配音与语音批注。",
      CFBundleDisplayName: "导演工作台",
    },
  },
});
