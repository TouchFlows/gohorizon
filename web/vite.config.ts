import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Where the Go service listens while developing. It is the address in
// examples/gohorizon.yaml; GOHORIZON_API overrides it for a service on another host or port.
const api = process.env.GOHORIZON_API ?? 'http://127.0.0.1:8090'

export default defineConfig({
  plugins: [react()],
  server: {
    // Components call /api/v1/... in both modes: in production the service serves the
    // bundle itself, and here the dev server forwards those paths to it, so no component
    // needs to know which one it is running in.
    proxy: {
      '/api': { target: api },
    },
  },
})
