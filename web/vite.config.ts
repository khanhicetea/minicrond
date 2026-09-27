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
  // Keep generated asset URLs relative so they work under any BASE_PATH.
  // The Go server places the HTML entry links under its /assets/ route.
  base: './',
  server: {
    // Dev convenience: forward API calls to a locally running daemon.
    proxy: { '/api': 'http://127.0.0.1:7423' },
  },
  build: {
    outDir: resolve(import.meta.dirname, '../internal/api/assets'),
    emptyOutDir: true,
    chunkSizeWarningLimit: 600,
    rollupOptions: {
      output: {
        // The whole SPA is embedded in the Go binary, so emit one JS file.
        codeSplitting: false,
        entryFileNames: 'app-[hash].js',
        assetFileNames: asset => asset.name?.endsWith('.css') ? 'style-[hash].css' : 'asset-[hash][extname]',
      },
    },
  },
});
