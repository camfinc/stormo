// The browser capability's worker: one headless Chromium, a fresh context per job or a kept one per
// agent session, serving the capability contract (docs/capabilities.md): GET /health once the
// browser is up, POST /run {job, agent, input, files, out} with what the steps read as the answer
// and screenshots or PDFs written to out.
import http from "node:http";
import dns from "node:dns/promises";
import net from "node:net";
import path from "node:path";
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
async function hostAllowed(host) {
  if (ALLOW_PRIVATE) return true;
  host = host.replace(/^\[|\]$/g, "");
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
async function vetted(host) {
  host = host.replace(/^\[|\]$/g, "");
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
  const ip = u.protocol === "http:" ? await vetted(u.hostname) : null;
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
  const ip = m ? await vetted(m[1]) : null;
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
    if (!["http:", "https:"].includes(url.protocol) || !(await hostAllowed(url.hostname))) return route.abort("blockedbyclient");
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

async function run(job) {
  const input = job.input || {};
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
