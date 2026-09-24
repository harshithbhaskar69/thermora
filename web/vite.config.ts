import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { resolve } from "node:path";

export default defineConfig({
  plugins: [react()],
  build: {
    rolldownOptions: {
      input: {
        app: resolve(import.meta.dirname, "index.html"),
        docs: resolve(import.meta.dirname, "docs/index.html"),
      },
    },
  },
  server: {
    proxy: {
      "/api": { target: "https://localhost:8443", secure: false },
      "/edge": { target: "https://localhost:8443", secure: false },
      "/health": { target: "https://localhost:8443", secure: false },
      "/openapi.yaml": { target: "https://localhost:8443", secure: false },
      "/manufacturer-openapi.yaml": {
        target: "https://localhost:8443",
        secure: false,
      },
    },
  },
});
