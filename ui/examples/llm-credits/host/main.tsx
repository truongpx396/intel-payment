// The page the browser tests load: the reference screen over a real server, through a same-origin proxy that holds the service
// credential — as a product's own backend would (rest-api.md: /v1 is for a host's backend, never for a browser). Nothing here is
// the package's: the scope comes from the address, the proxy is mounted at the origin.
import { createRoot } from "react-dom/client";
import { LlmCreditsScreen } from "../src/index.js";

const q = new URLSearchParams(window.location.search);
const realm = q.get("scope_realm");
const scope = { kind: q.get("scope_kind") ?? "workspace", id: q.get("scope_id") ?? "", ...(realm === null ? {} : { realm }) };
const root = document.getElementById("root");
if (root === null) throw new Error("the host page has no #root");

createRoot(root).render(<LlmCreditsScreen baseUrl="" scope={scope} currency={q.get("currency") ?? "USD"} returnedFromCheckout={q.get("checkout") === "done"} />);
