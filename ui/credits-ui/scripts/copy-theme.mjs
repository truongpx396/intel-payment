// The stylesheet is not compiled; it ships beside the JavaScript so `@intel-payment/credits-ui/theme.css` resolves.
import { copyFileSync, mkdirSync } from "node:fs";

mkdirSync(new URL("../dist", import.meta.url), { recursive: true });
copyFileSync(new URL("../src/theme.css", import.meta.url), new URL("../dist/theme.css", import.meta.url));
