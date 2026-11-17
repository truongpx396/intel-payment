import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { readFile } from 'node:fs/promises';
import { extname, resolve, sep } from 'node:path';

/**
 * A product's own backend, in miniature, for the browser tests: it serves the built reference page and forwards `/v1/*` to the stack under
 * test, adding the service credential. rest-api.md is explicit that `/v1` is for a host's BACKEND — a browser never holds the token — so the
 * test is shaped the way a deployment is: the page talks to its own origin, and the origin talks to the service.
 *
 * It forwards only what the binding sends (a method, a path under /v1, three headers and a body), never the browser's cookies, and it serves
 * nothing outside the built directory.
 */
export interface Host {
  readonly url: string;
  close(): Promise<void>;
}

const TYPES: Record<string, string> = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.map': 'application/json',
};
const FORWARD_HEADERS = ['accept', 'content-type', 'idempotency-key'];

async function body(req: IncomingMessage): Promise<Buffer | undefined> {
  if (req.method === 'GET' || req.method === 'HEAD') return undefined;
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return Buffer.concat(chunks);
}

async function forward(req: IncomingMessage, res: ServerResponse, upstream: string, token: string) {
  const headers: Record<string, string> = { authorization: `Bearer ${token}` };
  for (const h of FORWARD_HEADERS) {
    const v = req.headers[h];
    if (typeof v === 'string') headers[h] = v;
  }
  try {
    const payload = await body(req);
    const r = await fetch(new URL(req.url ?? '/', upstream), { method: req.method, headers, ...(payload === undefined ? {} : { body: new Uint8Array(payload) }) });
    res.writeHead(r.status, { 'content-type': r.headers.get('content-type') ?? 'application/json' });
    res.end(Buffer.from(await r.arrayBuffer()));
  } catch (e) {
    res.writeHead(502, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ error: { code: 'upstream_unreachable', message: e instanceof Error ? e.message : String(e) } }));
  }
}

async function serveStatic(req: IncomingMessage, res: ServerResponse, dir: string) {
  const path = new URL(req.url ?? '/', 'http://x').pathname;
  const file = resolve(dir, '.' + (path === '/' ? '/index.html' : path));
  if (file !== dir && !file.startsWith(dir + sep)) {
    res.writeHead(404).end();
    return;
  }
  try {
    const data = await readFile(file);
    res.writeHead(200, { 'content-type': TYPES[extname(file)] ?? 'application/octet-stream' }).end(data);
  } catch {
    res.writeHead(404).end();
  }
}

/** Starts the host on a free local port. `dir` is the built page (`npm run build:host` in ui/), `upstream` the stack under test. */
export async function startHost(o: { upstream: string; token: string; dir: string }): Promise<Host> {
  const dir = resolve(o.dir);
  const server: Server = createServer((req, res) => {
    const handle = (req.url ?? '').startsWith('/v1/') ? forward(req, res, o.upstream, o.token) : serveStatic(req, res, dir);
    handle.catch(() => {
      if (!res.headersSent) res.writeHead(500);
      res.end();
    });
  });
  await new Promise<void>((ok) => server.listen(0, '127.0.0.1', ok));
  const addr = server.address();
  if (addr === null || typeof addr === 'string') throw new Error('the test host has no port');
  return { url: `http://127.0.0.1:${addr.port}`, close: () => new Promise<void>((ok) => { server.close(() => ok()); server.closeAllConnections(); }) };
}
