import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import { resolve } from 'node:path';

export default defineConfig({
  plugins: [
    tailwindcss(),
    // React 19 with the React Compiler (native oxc port) for automatic memoization.
    react({ compiler: true }),
  ],
  // The Go server serves UI assets under /assets/ and exempts only that
  // prefix from bearer-token auth; index.html must reference them there.
  base: '/assets/',
  server: {
    // Dev convenience: forward API calls to a locally running daemon.
    proxy: { '/api': 'http://127.0.0.1:7423' },
  },
  build: {
    outDir: resolve(import.meta.dirname, '../internal/api/assets'),
    emptyOutDir: true,
    rollupOptions: {
      // The Go asset handler serves exactly app.js and style.css, so ship a
      // single JS chunk with no code splitting and no extra asset files.
      output: {
        codeSplitting: false,
        entryFileNames: 'app.js',
        chunkFileNames: 'chunk-[hash].js',
        assetFileNames: asset => asset.name?.endsWith('.css') ? 'style.css' : 'asset-[hash][extname]',
      },
    },
  },
});
