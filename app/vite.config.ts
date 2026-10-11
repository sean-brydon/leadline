import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": path.resolve(import.meta.dirname, "./src") } },
  // A desktop app loads from disk; one large chunk is fine.
  build: { chunkSizeWarningLimit: 2000 },
  // Tauri reads Rust errors from this terminal and expects a fixed port.
  // 1520, not Tauri's usual 1420: other apps' dev servers live there.
  clearScreen: false,
  server: {
    port: 1520,
    strictPort: true,
    watch: { ignored: ["**/src-tauri/**"] },
  },
});
