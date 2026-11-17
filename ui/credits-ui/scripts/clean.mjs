// A build starts from nothing, so a file deleted from src/ cannot linger in dist/ and be packed.
import { rmSync } from "node:fs";

rmSync(new URL("../dist", import.meta.url), { recursive: true, force: true });
