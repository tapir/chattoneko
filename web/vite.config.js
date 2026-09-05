import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  resolve: {
    alias: {
      $lib: path.resolve("./src/lib"),
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // The entry chunk is ~525 kB / ~149 kB gzipped: Svelte runtime, bits-ui,
    // vaul-svelte and the app's own components. It is a single-view chat app,
    // so there is no route to split it along and lazy-loading the sheets would
    // just delay the first interaction. Default 500 kB warning is noise here.
    chunkSizeWarningLimit: 600,
    rollupOptions: {
      output: {
        // Split the heavy markdown pipeline (marked + Temml + highlight.js
        // + DOMPurify, ~135 kB gzipped) out of the main bundle.
        //
        // Naming the chunk is only HALF the job: markdown.impl.js must also be
        // reached via dynamic import() (see src/lib/markdown.js), otherwise
        // rolldown emits it as a separate file that index.html still
        // <link rel="modulepreload">s, keeping every byte on the critical
        // path. A static import here silently undoes the whole split.
        //
        // Vite 8 (rolldown) only accepts the function form.
        manualChunks: (id) =>
          /node_modules\/(marked|temml|highlight\.js|dompurify)\//.test(id)
            ? "markdown"
            : undefined,
      },
    },
  },
  server: {
    proxy: {
      "/api": "http://localhost:8080",
    },
  },
});
