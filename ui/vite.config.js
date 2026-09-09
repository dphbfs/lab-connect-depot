import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  // Only used by `npm run dev` — the built app is served same-origin by
  // lab-connect-mcp in production, so this proxy exists purely for local
  // frontend development against a running backend on :4224.
  server: {
    proxy: {
      '/api': 'http://localhost:4224',
    },
  },
})
