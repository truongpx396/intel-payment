import { afterEach } from "vitest";

// Testing Library only auto-cleans when the runner exposes globals; this one does not, so do it by hand — and only where a DOM exists.
afterEach(async () => {
  if (typeof document === "undefined") return;
  const { cleanup } = await import("@testing-library/react");
  cleanup();
});
