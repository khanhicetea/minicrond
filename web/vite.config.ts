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
  // prefix (plus the SPA shell) from bearer-token auth; index.html must
  // reference them there.
  base: '/assets/',
  server: {
    // Dev convenience: forward API calls to a locally running daemon.
    proxy: { '/api': 'http://127.0.0.1:7423' },
  },
  build: {
    outDir: resolve(import.meta.dirname, '../internal/api/assets'),
    emptyOutDir: true,
    rollupOptions: {
      output: {
        entryFileNames: 'app-[hash].js',
        chunkFileNames: 'chunk-[hash].js',
        assetFileNames: asset => asset.name?.endsWith('.css') ? 'style-[hash].css' : 'asset-[hash][extname]',
      },
    },
  },
});
