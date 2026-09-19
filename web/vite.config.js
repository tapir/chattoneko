import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  resolve: {
    alias: [
      { find: "$lib", replacement: path.resolve("./src/lib") },
      // incremark-renderer imports the FULL highlight.js (~190 languages,
      // ~250 kB gzipped extra); the app only highlights the common set, so
      // point the bare specifier at it. Exact match: a string alias would
      // also rewrite "highlight.js/lib/common" into a doubled path.
      { find: /^highlight\.js$/, replacement: "highlight.js/lib/common" },
      // svelte-pdf points pdf.js's workerSrc at the UNMINIFIED worker via
      // `new URL("pdfjs-dist/build/pdf.worker.mjs", import.meta.url)`, which
      // Vite resolves and emits as a second 2.2 MB asset next to the minified
      // one lib/pdf.js loads (dist is embedded in the Go binary, so dead
      // weight costs binary size). Aliasing both specifiers to the same file
      // emits it once.
      {
        find: /^pdfjs-dist\/build\/pdf\.worker\.mjs$/,
        replacement: "pdfjs-dist/build/pdf.worker.min.mjs",
      },
    ],
  },
  optimizeDeps: {
    // svelte-pdf ships raw .svelte source (its dist/index.js re-exports one),
    // which the dep pre-bundler's esbuild cannot parse — vite-plugin-svelte
    // has to compile it instead.
    exclude: ["svelte-pdf"],
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // The entry chunk is ~550 kB / ~158 kB gzipped: Svelte runtime, bits-ui,
    // vaul-svelte and the app's own components. It is a single-view chat app,
    // so there is no route to split it along and lazy-loading the sheets would
    // just delay the first interaction. Default 500 kB warning is noise here.
    chunkSizeWarningLimit: 700,
    rollupOptions: {
      output: {
        // Split the heavy markdown pipeline (incremark-renderer and its
        // marked + katex + highlight.js + xss deps) out of the main bundle.
        //
        // Naming the chunk is only HALF the job: markdown.impl.js must also be
        // reached via dynamic import() (see src/lib/markdown.js), otherwise
        // rolldown emits it as a separate file that index.html still
        // <link rel="modulepreload">s, keeping every byte on the critical
        // path. A static import here silently undoes the whole split.
        //
        // Vite 8 (rolldown) only accepts the function form.
        manualChunks: (id) =>
          /node_modules\/(incremark-renderer|marked|katex|highlight\.js|xss)\//.test(id)
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
