import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';
import { mockApi } from './mock/plugin';

// Must match noStoreHTML in internal/api/ui.go, so `npm run preview` fails
// the same way the node would if the bundle ever needs inline script/style.
const CSP =
  "default-src 'none'; style-src 'self'; script-src 'self'; " +
  "connect-src *; img-src 'self' data:; form-action 'none'; frame-ancestors 'none'";

// Modes:
//   npm run dev      proxy /api + /healthz to LEAP_API (default an ssh -L tunnel on :18080)
//   npm run mock     fake in-memory node, no backend needed
//   npm run build    write the production bundle into internal/api/ui/ (go:embed)
//   npm run preview  serve that bundle with the production CSP + the mock API
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), 'LEAP_');
  const target = env.LEAP_API || 'http://127.0.0.1:18080';
  const mock = mode === 'mock';

  return {
    base: './',
    plugins: [react(), mock && mockApi()],
    build: {
      outDir: '../internal/api/ui',
      emptyOutDir: true,
      chunkSizeWarningLimit: 1500,
      // Stable names: the Go side serves with Cache-Control: no-store, so
      // content hashes buy nothing and would churn the diff on every build.
      rollupOptions: {
        output: {
          entryFileNames: 'assets/app.js',
          chunkFileNames: 'assets/[name].js',
          assetFileNames: (a) => (a.names?.[0]?.endsWith('.css') ? 'assets/app.css' : 'assets/[name][extname]'),
        },
      },
    },
    server: {
      proxy: mock ? undefined : { '/api': target, '/healthz': target },
    },
    preview: {
      headers: { 'Content-Security-Policy': CSP },
    },
  };
});
