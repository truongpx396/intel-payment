// The credits-ui boundary, as ESLint rules (credits-ui-ports.md, "Extraction-ready code organization"). Run from ui/.
// scripts/verify-ui-boundary.sh plants a violation per rule and requires each to be rejected, so none of this can be
// unarmed without the build saying so.
import js from "@eslint/js";
import { defineConfig, globalIgnores } from "eslint/config";
import { createTypeScriptImportResolver } from "eslint-import-resolver-typescript";
import { importX } from "eslint-plugin-import-x";
import tseslint from "typescript-eslint";

const basePath = import.meta.dirname;

// A .ts file in the package is the framework-free half (ports, the model); a .tsx file is where React is allowed. The
// distinction is the whole rule, so there is no list of files to keep in step.
const noFramework = {
  patterns: [
    {
      group: ["react", "react/*", "react-dom", "react-dom/*"],
      message: "A .ts file in credits-ui is framework-free: ports.ts and model.ts carry no React. Bind to React in a .tsx component.",
    },
  ],
};
const noTransport = ["fetch", "XMLHttpRequest", "WebSocket", "window", "document", "localStorage", "sessionStorage", "navigator", "location"].map(
  (name) => ({ name, message: "A .ts file in credits-ui is pure: no transport, no store, no DOM. The CreditsSource is the only way in." }),
);

export default defineConfig([
  globalIgnores(["**/dist/**", "**/node_modules/**"]),
  js.configs.recommended,
  tseslint.configs.recommended,
  {
    files: ["**/*.{ts,tsx}"],
    plugins: { "import-x": importX },
    settings: { "import-x/resolver-next": [createTypeScriptImportResolver({ alwaysTryTypes: true })] },
    rules: {
      "import-x/no-restricted-paths": [
        "error",
        {
          basePath,
          zones: [
            {
              target: "./credits-ui/**/*",
              from: "./examples",
              message: "credits-ui may not import the reference binding: everything product-specific lives in examples/, and the package must build without it.",
            },
            {
              target: "./examples/**/*",
              from: "./credits-ui",
              except: ["./dist"],
              message: "A binding uses the package through its entry point (@intel-payment/credits-ui), the way a host would. It does not reach into the package's source.",
            },
          ],
        },
      ],
    },
  },
  {
    files: ["credits-ui/src/**/*.ts"],
    rules: { "no-restricted-imports": ["error", noFramework], "no-restricted-globals": ["error", ...noTransport] },
  },
  { files: ["**/*.mjs"], languageOptions: { globals: { URL: "readonly", process: "readonly" } } },
]);
