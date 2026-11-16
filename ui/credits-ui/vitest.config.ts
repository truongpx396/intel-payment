import { defineConfig } from "vitest/config";

// Model and type tests run in `node`: the pure half of the package must not need a DOM. A component test opts in with
// `// @vitest-environment jsdom` at the top of the file.
export default defineConfig({
  test: {
    include: ["test/**/*.test.{ts,tsx}"],
    environment: "node",
    setupFiles: ["./test/setup.ts"],
    typecheck: { enabled: true, include: ["test/**/*.test-d.ts"], tsconfig: "./tsconfig.json" },
  },
});
