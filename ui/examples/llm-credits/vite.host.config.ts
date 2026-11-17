import { defineConfig } from "vite";

// Builds the host page the browser tests load (host/). It is a test fixture, not the package and not the binding's own entry:
// `npm run build:host -w @intel-payment/example-llm-credits` writes dist-host/, which e2e/ui-host serves.
export default defineConfig({
  root: new URL("./host", import.meta.url).pathname,
  base: "/",
  build: { outDir: new URL("./dist-host", import.meta.url).pathname, emptyOutDir: true },
});
