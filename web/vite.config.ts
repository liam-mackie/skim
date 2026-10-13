import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';
import { fileURLToPath, URL } from 'node:url';

// The Go server serves /api same-origin (default 127.0.0.1:7437). In dev, Vite proxies /api to it.
// The dev UI origin is fixed at http://localhost:5173; start the server with
// `--dev-origin http://localhost:5173` (or SKIM_DEV_ORIGIN) so its origin check accepts it.
const target = process.env.SKIM_API ?? 'http://127.0.0.1:7437';

export default defineConfig({
  plugins: [tailwindcss(), svelte()],
  resolve: { alias: { $lib: fileURLToPath(new URL('./src/lib', import.meta.url)) } },
  server: {
    host: 'localhost',
    port: 5173,
    strictPort: true,
    proxy: { '/api': { target, changeOrigin: true } },
  },
  preview: { host: 'localhost', port: 4173, strictPort: true, proxy: { '/api': { target, changeOrigin: true } } },
});
