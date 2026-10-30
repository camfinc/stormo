// The browser capability's worker: one headless Chromium, a fresh context per job or a kept one per
// agent session, serving the capability contract (docs/capabilities.md): GET /health once the
// browser is up, POST /run {job, agent, input, files, out} with what the steps read as the answer
// and screenshots or PDFs written to out.
import http from "node:http";
import dns from "node:dns/promises";
import net from "node:net";
import path from "node:path";
import fs from "node:fs/promises";
import { createReadStream } from "node:fs";
import { chromium } from "playwright-core";

const PORT = Number(process.env.PORT || 8080);
const ALLOW_PRIVATE = process.env.BROWSER_ALLOW_PRIVATE === "1";
const SESSION_IDLE_MS = 15 * 60 * 1000;
const MAX_SESSIONS = 5; // per agent
const MAX_TEXT = 100 * 1024;
const MAX_LINKS = 200;

let browser;
const sessions = new Map(); // `${agent}\0${name}` → {context, page, last}

// Private, loopback, link-local and similar addresses stay out of reach unless allowed: a page must
// not become a way into the host's network.
function privateIP(ip) {
  if (net.isIPv4(ip)) {
    const [a, b] = ip.split(".").map(Number);
    return a === 10 || a === 127 || a === 0 || (a === 169 && b === 254) || (a === 172 && b >= 16 && b <= 31) ||
      (a === 192 && b === 168) || (a === 100 && b >= 64 && b <= 127) || a >= 224;
  }
  const v = ip.toLowerCase();
  if (v.startsWith("::ffff:")) return privateIP(v.slice(7));
  return v === "::1" || v === "::" || v.startsWith("fc") || v.startsWith("fd") || v.startsWith("fe8") || v.startsWith("fe9") ||
    v.startsWith("fea") || v.startsWith("feb") || v.startsWith("ff");
}

const verdicts = new Map(); // host → {ok, at}
async function hostAllowed(host, port) {
  host = host.replace(/^\[|\]$/g, "");
  if (host === "127.0.0.1" && pageServers.has(Number(port))) return true;
  if (ALLOW_PRIVATE) return true;
  if (host === "localhost" || host.endsWith(".localhost") || host.endsWith(".internal") || host.endsWith(".local")) return false;
  if (net.isIP(host)) return !privateIP(host);
  const seen = verdicts.get(host);
  if (seen && Date.now() - seen.at < 60_000) return seen.ok;
  let ok = false;
  try {
    const addrs = await dns.lookup(host, { all: true });
    ok = addrs.length > 0 && addrs.every((a) => !privateIP(a.address));
  } catch {
    ok = false;
  }
  verdicts.set(host, { ok, at: Date.now() });
  return ok;
}

// Every connection the browser makes goes through this proxy (redirects, subresources, websockets
// included), which resolves the host itself, refuses private addresses and connects to the address
// it checked, so a redirect or a DNS answer that changes cannot reach the host's network.
const PROXY_PORT = 3128;
// A page job's own static server (127.0.0.1:<port>, its handed-over folder only) is the one
// loopback address a page may reach, and only while that job runs.
const pageServers = new Set();

async function vetted(host, port) {
  host = host.replace(/^\[|\]$/g, "");
  if (host === "127.0.0.1" && pageServers.has(Number(port))) return host;
  if (ALLOW_PRIVATE) return host;
  if (net.isIP(host)) return privateIP(host) ? null : host;
  if (host === "localhost" || host.endsWith(".localhost")) return null;
  try {
    const addrs = await dns.lookup(host, { all: true });
    return addrs.length && addrs.every((a) => !privateIP(a.address)) ? addrs[0].address : null;
  } catch {
    return null;
  }
}

const proxy = http.createServer(async (req, res) => {
  let u;
  try {
    u = new URL(req.url);
  } catch {
    return res.writeHead(400).end();
  }
  const ip = u.protocol === "http:" ? await vetted(u.hostname, u.port || 80) : null;
  if (!ip) return res.writeHead(403, { "content-type": "text/plain" }).end("blocked: a private address");
  const headers = { ...req.headers, host: u.host };
  delete headers["proxy-connection"];
  const up = http.request({ host: ip, port: u.port || 80, method: req.method, path: u.pathname + u.search, headers, setHost: false }, (r) => {
    res.writeHead(r.statusCode, r.headers);
    r.pipe(res);
  });
  up.on("error", () => (res.headersSent ? res.destroy() : res.writeHead(502).end()));
  req.pipe(up);
});
proxy.on("connect", async (req, sock, head) => {
  const m = /^(\[[^\]]+\]|[^:]+):(\d+)$/.exec(req.url || "");
  const ip = m ? await vetted(m[1], m[2]) : null;
  if (!ip) return sock.end("HTTP/1.1 403 Forbidden\r\n\r\n");
  const up = net.connect(Number(m[2]), ip, () => {
    sock.write("HTTP/1.1 200 Connection Established\r\n\r\n");
    if (head?.length) up.write(head);
    up.pipe(sock);
    sock.pipe(up);
  });
  up.on("error", () => sock.destroy());
  sock.on("error", () => up.destroy());
});

async function guard(context) {
  await context.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (["data:", "blob:", "about:"].includes(url.protocol)) return route.continue();
    if (!["http:", "https:"].includes(url.protocol) || !(await hostAllowed(url.hostname, url.port || (url.protocol === "https:" ? 443 : 80)))) return route.abort("blockedbyclient");
    return route.continue();
  });
}

async function newContext(input) {
  const vp = input.viewport || {};
  const context = await browser.newContext({
    viewport: { width: clampInt(vp.width, 320, 3840, 1280), height: clampInt(vp.height, 240, 2160, 800) },
    acceptDownloads: false,
  });
  await guard(context);
  return context;
}

function clampInt(v, lo, hi, d) {
  const n = Number.isInteger(v) ? v : d;
  return Math.min(hi, Math.max(lo, n));
}

async function pageFor(agent, input) {
  if (!input.session) {
    const context = await newContext(input);
    return { context, page: await context.newPage(), close: () => context.close() };
  }
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(input.session)) throw new Error("session is a name of letters, digits, dots, hyphens or underscores");
  const key = `${agent}\0${input.session}`;
  let s = sessions.get(key);
  if (!s || s.page.isClosed()) {
    const mine = [...sessions.keys()].filter((k) => k.startsWith(`${agent}\0`));
    if (mine.length >= MAX_SESSIONS) {
      const oldest = mine.sort((a, b) => sessions.get(a).last - sessions.get(b).last)[0];
      await sessions.get(oldest).context.close().catch(() => {});
      sessions.delete(oldest);
    }
    const context = await newContext(input);
    s = { context, page: await context.newPage(), last: Date.now() };
    sessions.set(key, s);
  }
  s.last = Date.now();
  return { context: s.context, page: s.page, close: async () => { s.last = Date.now(); } };
}

setInterval(async () => {
  for (const [k, s] of sessions) {
    if (Date.now() - s.last > SESSION_IDLE_MS) {
      sessions.delete(k);
      await s.context.close().catch(() => {});
    }
  }
}, 60_000).unref();

function clip(s, n) {
  return s.length > n ? s.slice(0, n) : s;
}

function fileName(name, ext, i) {
  const base = (name || `step-${i + 1}`).replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^[.-]+/, "") || `step-${i + 1}`;
  return base.endsWith(ext) ? base : base + ext;
}

async function readPage(page) {
  const text = await page.evaluate(() => (document.body ? document.body.innerText : "")).catch(() => "");
  const links = await page.$$eval("a[href]", (as) => as.map((a) => ({ text: (a.innerText || "").trim().slice(0, 200), href: a.href }))).catch(() => []);
  return { text: clip(text, MAX_TEXT), links: links.filter((l) => /^https?:/.test(l.href)).slice(0, MAX_LINKS), truncated: text.length > MAX_TEXT };
}

async function step(page, s, i, out) {
  const sel = s.selector;
  const need = (v, what) => {
    if (v === undefined || v === null || v === "") throw new Error(`step ${i + 1} (${s.action}) needs ${what}`);
    return v;
  };
  switch (s.action) {
    case "goto": {
      const res = await page.goto(need(s.url, "url"), { waitUntil: "load" });
      return { status: res ? res.status() : null, url: page.url() };
    }
    case "click":
      await page.click(need(sel, "selector"));
      return {};
    case "fill":
      await page.fill(need(sel, "selector"), String(s.value ?? ""));
      return {};
    case "press":
      await page.press(need(sel, "selector"), need(s.value, "value (a key)"));
      return {};
    case "select":
      return { selected: await page.selectOption(need(sel, "selector"), String(need(s.value, "value"))) };
    case "check":
      await page.check(need(sel, "selector"));
      return {};
    case "hover":
      await page.hover(need(sel, "selector"));
      return {};
    case "scroll":
      if (sel) await page.locator(sel).first().scrollIntoViewIfNeeded();
      else await page.mouse.wheel(0, Number(s.value) || 800);
      return {};
    case "wait":
      await page.waitForTimeout(Math.min(Number(s.ms) || 1000, 30_000));
      return {};
    case "wait_for":
      await page.waitForSelector(need(sel, "selector"));
      return {};
    case "text": {
      const t = sel ? await page.locator(sel).first().innerText() : (await readPage(page)).text;
      return { text: clip(t, MAX_TEXT), truncated: t.length > MAX_TEXT };
    }
    case "html": {
      const h = sel ? await page.locator(sel).first().evaluate((e) => e.outerHTML) : await page.content();
      return { html: clip(h, MAX_TEXT), truncated: h.length > MAX_TEXT };
    }
    case "links":
      return { links: (await readPage(page)).links };
    case "eval": {
      const v = await page.evaluate(need(s.value, "value (an expression)"));
      const j = JSON.stringify(v ?? null);
      return { value: j.length > MAX_TEXT ? clip(j, MAX_TEXT) : v ?? null, truncated: j.length > MAX_TEXT };
    }
    case "screenshot": {
      if (!out) throw new Error("no out folder for files");
      const name = fileName(s.name, ".png", i);
      if (sel) await page.locator(sel).first().screenshot({ path: path.join(out, name) });
      else await page.screenshot({ path: path.join(out, name), fullPage: !!s.full_page });
      return { file: name };
    }
    case "pdf": {
      if (!out) throw new Error("no out folder for files");
      const name = fileName(s.name, ".pdf", i);
      await page.pdf({ path: path.join(out, name), printBackground: true });
      return { file: name };
    }
    default:
      throw new Error(`step ${i + 1}: unknown action ${JSON.stringify(s.action)}`);
  }
}

const TYPES = { ".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8", ".css": "text/css", ".js": "text/javascript", ".mjs": "text/javascript",
  ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
  ".webp": "image/webp", ".avif": "image/avif", ".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
  ".mp4": "video/mp4", ".webm": "video/webm", ".mp3": "audio/mpeg", ".wav": "audio/wav" };

// serveFolder serves one folder on a loopback port of its own: nothing outside it, no listing.
function serveFolder(root) {
  return new Promise((ok, fail) => {
    const srv = http.createServer(async (req, res) => {
      const rel = decodeURIComponent(new URL(req.url, "http://x").pathname);
      const p = path.resolve(root, "." + rel);
      if (p !== root && !p.startsWith(root + path.sep)) return res.writeHead(403).end();
      try {
        const real = await fs.realpath(p);
        if (real !== root && !real.startsWith(root + path.sep)) return res.writeHead(403).end();
        const st = await fs.stat(real);
        if (!st.isFile()) return res.writeHead(404).end();
        res.writeHead(200, { "content-type": TYPES[path.extname(real).toLowerCase()] || "application/octet-stream", "content-length": st.size });
        createReadStream(real).pipe(res);
      } catch {
        res.writeHead(404).end();
      }
    });
    srv.on("error", fail);
    srv.listen(0, "127.0.0.1", () => ok(srv));
  });
}

// renderPage renders an HTML page of a handed-over folder to an image (or PDF) of an exact size.
async function renderPage(job) {
  const input = job.input || {};
  if (!job.files?.length) throw Object.assign(new Error("hand over the page's folder as the first file"), { code: 400 });
  if (!job.out) throw Object.assign(new Error("no out folder for the image"), { code: 400 });
  const root = await fs.realpath(job.files[0]);
  if (!(await fs.stat(root)).isDirectory()) throw Object.assign(new Error("the first file must be the page's folder"), { code: 400 });
  const rel = String(input.page || "index.html").replace(/^\/+/, "");
  const html = await fs.readFile(path.resolve(root, rel), "utf8").catch(() => null);
  if (html === null || !path.resolve(root, rel).startsWith(root + path.sep)) throw Object.assign(new Error(`no page ${rel} in the folder`), { code: 400 });
  const meta = /<meta\s+name="still-size"\s+content="(\d+)x(\d+)"/.exec(html);
  const width = clampInt(input.width ?? (meta && Number(meta[1])), 16, 8000, 1080);
  const height = clampInt(input.height ?? (meta && Number(meta[2])), 16, 8000, 1350);
  const format = ["png", "jpeg", "pdf"].includes(input.format) ? input.format : "png";
  const srv = await serveFolder(root);
  const port = srv.address().port;
  pageServers.add(port);
  const context = await browser.newContext({ viewport: { width, height }, deviceScaleFactor: 1 });
  try {
    await guard(context);
    const page = await context.newPage();
    page.setDefaultTimeout(clampInt(input.timeout_seconds, 1, 120, 30) * 1000);
    const failed = [];
    page.on("requestfailed", (r) => failed.length < 20 && failed.push(`${r.url()}: ${r.failure()?.errorText}`));
    await page.goto(`http://127.0.0.1:${port}/${rel.split("/").map(encodeURIComponent).join("/")}`, { waitUntil: "networkidle" });
    await page.evaluate(() => document.fonts && document.fonts.ready);
    await page.waitForTimeout(Math.min(Math.max(Number(input.wait_ms) || 300, 0), 10_000));
    const size = await page.evaluate(() => ({ width: document.documentElement.scrollWidth, height: document.documentElement.scrollHeight }));
    const name = fileName(input.name || path.basename(rel, path.extname(rel)), format === "jpeg" ? ".jpg" : "." + format, 0);
    if (format === "pdf") await page.pdf({ path: path.join(job.out, name), width: `${width}px`, height: `${height}px`, printBackground: true });
    else await page.screenshot({ path: path.join(job.out, name), type: format, quality: format === "jpeg" ? 92 : undefined, clip: { x: 0, y: 0, width, height } });
    return { file: name, files: [name], width, height, page_size: size, fits: size.width <= width && size.height <= height, failed_requests: failed };
  } finally {
    pageServers.delete(port);
    srv.close();
    await context.close().catch(() => {});
  }
}

async function run(job) {
  const input = job.input || {};
  if (input.page !== undefined) return renderPage(job);
  const steps = Array.isArray(input.steps) ? input.steps : [];
  if (!input.url && !steps.length) throw Object.assign(new Error("give a url, steps, or both"), { code: 400 });
  if (steps.length > 100) throw Object.assign(new Error("at most 100 steps"), { code: 400 });
  const { page, close } = await pageFor(job.agent, input);
  page.setDefaultTimeout(clampInt(input.timeout_seconds, 1, 120, 30) * 1000);
  const files = [];
  try {
    if (input.url) await step(page, { action: "goto", url: input.url }, -1, job.out);
    const answer = { url: page.url(), title: await page.title().catch(() => "") };
    if (!steps.length) Object.assign(answer, await readPage(page));
    const results = [];
    for (let i = 0; i < steps.length; i++) {
      try {
        const r = await step(page, steps[i], i, job.out);
        if (r.file) files.push(r.file);
        results.push({ action: steps[i].action, ok: true, ...r });
      } catch (e) {
        results.push({ action: steps[i].action, ok: false, error: String(e.message || e).split("\n")[0] });
        break; // later steps depend on this one
      }
    }
    if (input.screenshot && job.out) {
      const name = fileName("final", ".png", 0);
      await page.screenshot({ path: path.join(job.out, name), fullPage: true });
      files.push(name);
    }
    if (steps.length) answer.results = results;
    answer.url = page.url();
    answer.title = await page.title().catch(() => answer.title);
    answer.files = files;
    return answer;
  } finally {
    await close();
  }
}

const server = http.createServer(async (req, res) => {
  const send = (code, body) => {
    res.writeHead(code, { "content-type": code === 200 ? "application/json" : "text/plain" });
    res.end(code === 200 ? JSON.stringify(body) : String(body));
  };
  if (req.method === "GET" && req.url === "/health") return browser?.isConnected() ? send(200, { ok: true }) : send(503, "starting");
  if (req.method !== "POST" || req.url !== "/run") return send(404, "not found");
  let raw = "";
  for await (const chunk of req) {
    raw += chunk;
    if (raw.length > 1 << 20) return send(413, "body over 1 MB");
  }
  try {
    send(200, await run(JSON.parse(raw)));
  } catch (e) {
    send(e.code === 400 ? 400 : 422, String(e.message || e).split("\n")[0]);
  }
});

await new Promise((ok) => proxy.listen(PROXY_PORT, "127.0.0.1", ok));
browser = await chromium.launch({
  args: ["--no-sandbox", "--disable-gpu"], // the container is the sandbox
  // `<-loopback>`: loopback goes through the proxy too, where it is refused.
  proxy: { server: `http://127.0.0.1:${PROXY_PORT}`, bypass: "<-loopback>" },
});
browser.on("disconnected", () => process.exit(1)); // the core boots a fresh worker
server.listen(PORT, () => console.log(`browser: listening on :${PORT}`));
