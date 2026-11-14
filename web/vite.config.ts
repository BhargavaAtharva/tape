import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In development the API is proxied so the app can use relative URLs.
const target = process.env.TAPE_DEV_API ?? "http://localhost:8081";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5174,
    proxy: {
      "/v1": { target, ws: true, changeOrigin: true },
    },
  },
});
