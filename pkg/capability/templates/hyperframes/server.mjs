// The hyperframes capability's worker: the pinned HyperFrames CLI run on a handed-over project
// folder, for the capability contract (docs/capabilities.md): GET /health, POST /run
// {job, agent, input, files, out}. Only lint, validate, inspect, compositions, info and render:
// never publish, auth, upgrade or preview (uploads, accounts, unpinned versions, servers).
import http from "node:http";
import path from "node:path";
import fs from "node:fs/promises";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

const PORT = Number(process.env.PORT || 8080);
const CLI = path.join(path.dirname(fileURLToPath(import.meta.url)), "node_modules", "hyperframes", "bin", "hyperframes.mjs");
const READ_OPS = new Set(["lint", "validate", "inspect", "compositions", "info"]);
const QUALITY = new Set(["draft", "looks", "delivery", "standard", "high"]);
const FORMATS = { mp4: ".mp4", webm: ".webm", mov: ".mov", gif: ".gif" };

class JobError extends Error {
  constructor(msg, code = 422) {
    super(msg);
    this.code = code;
  }
}

function cli(args, cwd, signal) {
  return new Promise((ok) => {
    const p = spawn(process.execPath, [CLI, ...args], { cwd, env: { ...process.env, NO_COLOR: "1", FORCE_COLOR: "0" }, signal });
    let out = "", err = "";
    p.stdout.on("data", (b) => { out = (out + b).slice(-2_000_000); });
    p.stderr.on("data", (b) => { err = (err + b).slice(-200_000); });
    p.on("close", (code) => ok({ code, out, err }));
    p.on("error", (e) => ok({ code: -1, out, err: String(e) }));
  });
}

const strip = (s) => s.replace(/\x1b\[[0-9;]*m/g, "");
const tail = (s, n = 1500) => strip(s).trim().slice(-n);

function parsed(s) {
  const t = strip(s).trim();
  try {
    return JSON.parse(t);
  } catch {
    const i = t.indexOf("{");
    try {
      return i >= 0 ? JSON.parse(t.slice(i)) : null;
    } catch {
      return null;
    }
  }
}

async function run(job, signal) {
  const input = job.input || {};
  const op = input.op || "render";
  if (!job.files?.length) throw new JobError("hand over the HyperFrames project's folder as the first file", 400);
  const dir = job.files[0];
  if (!(await fs.stat(dir)).isDirectory()) throw new JobError("the first file must be the project's folder", 400);
  if (READ_OPS.has(op)) {
    const args = [op, ...(op === "lint" || op === "validate" || op === "inspect" ? ["--json"] : []), dir];
    const r = await cli(args, dir, signal);
    const j = parsed(r.out);
    return { op, ok: r.code === 0, exit_code: r.code, result: j ?? tail(r.out, 20000), ...(r.code === 0 ? {} : { error: tail(r.err || r.out) }) };
  }
  if (op !== "render") throw new JobError("op is render, lint, validate, inspect, compositions or info", 400);
  if (!job.out) throw new JobError("no out folder for the video", 400);
  const format = input.format || "mp4";
  if (!FORMATS[format]) throw new JobError("format is mp4, webm, mov or gif", 400);
  const quality = input.quality || "draft";
  if (!QUALITY.has(quality)) throw new JobError("quality is draft, looks, delivery, standard or high", 400);
  const name = String(input.name || "render").replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^[.-]+/, "") || "render";
  const outFile = path.join(job.out, name + FORMATS[format]);
  const args = ["render", dir, "-o", outFile, "-q", quality, "--format", format, "--quiet", "--no-browser-gpu", "--workers", String(input.workers || "auto")];
  if (input.composition) {
    const c = String(input.composition);
    if (c.includes("..") || path.isAbsolute(c)) throw new JobError("composition is a file inside the project", 400);
    args.push("-c", c);
  }
  if (input.fps) args.push("-f", String(input.fps));
  if (Number.isInteger(input.crf)) args.push("--crf", String(input.crf));
  if (input.variables && typeof input.variables === "object") args.push("--variables", JSON.stringify(input.variables));
  if (input.strict) args.push("--strict");
  const started = Date.now();
  const r = await cli(args, dir, signal);
  const st = await fs.stat(outFile).catch(() => null);
  if (r.code !== 0 || !st) throw new JobError(`render failed (exit ${r.code}): ${tail(r.err || r.out)}`);
  return { op, file: path.basename(outFile), files: [path.basename(outFile)], format, quality, size_mb: Math.round(st.size / 1e4) / 100,
    seconds: Math.round((Date.now() - started) / 100) / 10, log: tail(r.out, 1500) };
}

const server = http.createServer(async (req, res) => {
  const send = (code, body) => {
    res.writeHead(code, { "content-type": code === 200 ? "application/json" : "text/plain" });
    res.end(code === 200 ? JSON.stringify(body) : String(body));
  };
  if (req.method === "GET" && req.url === "/health") return send(200, { ok: true });
  if (req.method !== "POST" || req.url !== "/run") return send(404, "not found");
  let raw = "";
  for await (const chunk of req) {
    raw += chunk;
    if (raw.length > 1 << 20) return send(413, "body over 1 MB");
  }
  const ac = new AbortController();
  res.on("close", () => { if (!res.writableEnded) ac.abort(); }); // the core cancelled the job
  try {
    send(200, await run(JSON.parse(raw), ac.signal));
  } catch (e) {
    send(e.code || 500, String(e.message || e));
  }
});
server.listen(PORT, () => console.log(`hyperframes: listening on :${PORT}`));
