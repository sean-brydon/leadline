import { defineConfig } from "astro/config";

// 4390, not Astro's usual 4321: Leadline's daemon and its tests live in the
// low 4320s.
export default defineConfig({
  server: { port: 4390 },
  devToolbar: { enabled: false },
});
