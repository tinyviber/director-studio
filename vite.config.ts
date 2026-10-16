import { defineConfig } from "vite";

export default defineConfig({
  root: "web",
  server: {
    port: 5174,
    strictPort: true,
    watch: { ignored: ["**/.mygo/**", "**/build/**"] },
  },
  build: { outDir: "../dist", emptyOutDir: true },
});
