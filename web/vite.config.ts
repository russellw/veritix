import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    // The bundle is embedded in the binary and served from it, so nothing is
    // fetched from anywhere else. Sourcemaps are left off: they would ship the
    // whole front end's source to every customer install for no benefit.
    sourcemap: false,
  },
  server: {
    // In development the SPA is served by Vite and the API by `veritix serve`,
    // so the API is proxied to keep every fetch same-origin. Production serves
    // both from the one binary, and the app cannot tell the difference.
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
})
