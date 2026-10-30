// The video capability's worker: renders a Remotion project handed over as the job's first file,
// bundled against the packages installed here, then optionally masters its loudness and grabs
// stills. Capability contract (docs/capabilities.md): GET /health, POST /run {job, agent, input,
// files, out}.
import http from "node:http";
import os from "node:os";
import path from "node:path";
import fs from "node:fs/promises";
import { existsSync } from "node:fs";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { bundle } from "@remotion/bundler";
import { ensureBrowser, renderMedia, selectComposition } from "@remotion/renderer";

const run = promisify(execFile);
const PORT = Number(process.env.PORT || 8080);
const HERE = path.dirname(fileURLToPath(import.meta.url));
const MODULES = path.join(HERE, "node_modules");
const ENTRIES = ["src/index.ts", "src/index.tsx", "src/index.js", "src/index.jsx", "remotion/index.ts", "remotion/index.tsx"];
const CODECS = { h264: "mp4", h265: "mp4", vp9: "webm", prores: "mov", gif: "gif" };
let ready = false;

class JobError extends Error {
  constructor(msg, code = 422) {
    super(msg);
    this.code = code;
  }
}

function cpuCount() {
  const set = Number(process.env.RENDER_CONCURRENCY);
  if (set > 0) return set;
  return Math.max(1, Math.floor((os.availableParallelism?.() ?? os.cpus().length) / 2));
}

function safeName(s, d) {
  const n = String(s || d).replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^[.-]+/, "");
  return n || d;
}

async function ffprobe(file) {
  const { stdout } = await run("ffprobe", ["-v", "error", "-show_entries", "stream=codec_type", "-of", "json", file]);
  return JSON.parse(stdout).streams || [];
}

// Two-pass loudnorm to the target, the video stream copied as rendered.
async function master(file, lufs, tp) {
  const measure = await run("ffmpeg", ["-hide_banner", "-nostats", "-i", file, "-af", `loudnorm=I=${lufs}:TP=${tp}:LRA=11:print_format=json`, "-f", "null", "-"],
    { maxBuffer: 16 << 20 }).catch((e) => e);
  const err = measure.stderr || "";
  const m = JSON.parse(err.slice(err.lastIndexOf("{"), err.lastIndexOf("}") + 1));
  const tmp = file.replace(/(\.[a-z0-9]+)$/, ".master$1");
  const af = `loudnorm=I=${lufs}:TP=${tp}:LRA=11:measured_I=${m.input_i}:measured_TP=${m.input_tp}:measured_LRA=${m.input_lra}:measured_thresh=${m.input_thresh}:offset=${m.target_offset}:linear=true:print_format=json`;
  const second = await run("ffmpeg", ["-hide_banner", "-nostats", "-y", "-i", file, "-c:v", "copy", "-af", af, "-ar", "48000", "-c:a", "aac", "-b:a", "192k", tmp],
    { maxBuffer: 16 << 20 });
  await fs.rename(tmp, file);
  const e2 = second.stderr || "";
  const r = JSON.parse(e2.slice(e2.lastIndexOf("{"), e2.lastIndexOf("}") + 1));
  return { lufs: Number(r.output_i), true_peak: Number(r.output_tp) };
}

async function render(job) {
  const input = job.input || {};
  if (!input.composition) throw new JobError("composition is required: the id of the composition to render", 400);
  if (!job.files?.length) throw new JobError("hand over the Remotion project's folder as the first file", 400);
  const project = job.files[0];
  if (!(await fs.stat(project)).isDirectory()) throw new JobError("the first file must be the project's folder", 400);
  const entry = input.entry ? path.join(project, input.entry) : ENTRIES.map((e) => path.join(project, e)).find((p) => existsSync(p));
  if (!entry || !existsSync(entry) || !entry.startsWith(project + path.sep)) {
    throw new JobError(`no entry file: give entry (one of ${ENTRIES.join(", ")} by default)`, 400);
  }
  const codec = input.codec || "h264";
  if (!CODECS[codec]) throw new JobError(`codec is one of ${Object.keys(CODECS).join(", ")}`, 400);
  if (!job.out) throw new JobError("no out folder", 400);
  const name = safeName(input.name, input.composition);
  const outFile = path.join(job.out, `${name}.${CODECS[codec]}`);
  const props = input.props && typeof input.props === "object" ? input.props : {};

  let serveUrl;
  try {
    serveUrl = await bundle({
      entryPoint: entry,
      rootDir: project,
      publicDir: path.join(project, "public"),
      outDir: path.join(os.tmpdir(), `bundle-${job.job}`),
      // The project brings no node_modules: imports resolve to the packages installed here.
      webpackOverride: (c) => ({
        ...c,
        resolve: { ...c.resolve, modules: [path.join(project, "node_modules"), MODULES, "node_modules"] },
        resolveLoader: { ...(c.resolveLoader || {}), modules: [MODULES, "node_modules"] },
      }),
    });
  } catch (e) {
    throw new JobError(`bundling failed: ${String(e.message || e).slice(0, 1500)}`);
  }
  try {
    let composition;
    try {
      composition = await selectComposition({ serveUrl, id: input.composition, inputProps: props });
    } catch (e) {
      throw new JobError(`composition ${input.composition}: ${String(e.message || e).split("\n")[0]}`);
    }
    const opts = { composition, serveUrl, codec, outputLocation: outFile, inputProps: props, concurrency: cpuCount(), logLevel: "warn" };
    if (Number.isInteger(input.crf)) opts.crf = input.crf;
    if (typeof input.scale === "number" && input.scale > 0 && input.scale <= 4) opts.scale = input.scale;
    if (typeof input.frames === "string" && /^\d+(-\d+)?$/.test(input.frames)) {
      const [a, b] = input.frames.split("-").map(Number);
      opts.frameRange = b === undefined ? a : [a, b];
    }
    let frames = 0;
    opts.onProgress = ({ renderedFrames }) => { frames = renderedFrames; };
    try {
      await renderMedia(opts);
    } catch (e) {
      throw new JobError(`render failed: ${String(e.message || e).slice(0, 1500)}`);
    }
    const answer = {
      file: path.basename(outFile),
      width: Math.round(composition.width * (opts.scale || 1)),
      height: Math.round(composition.height * (opts.scale || 1)),
      fps: composition.fps,
      frames,
      duration_seconds: Math.round((frames / composition.fps) * 1000) / 1000,
      codec,
      stills: [],
    };
    const m = input.master;
    if (m && typeof m === "object" && codec !== "gif") {
      const streams = await ffprobe(outFile);
      if (streams.some((s) => s.codec_type === "audio")) {
        answer.loudness = await master(outFile, Number(m.lufs ?? -16), Number(m.true_peak ?? -1.5));
      } else {
        answer.loudness = null;
        answer.note = "no audio to master";
      }
    }
    if (Array.isArray(input.stills) && input.stills.length) {
      for (const t of input.stills.slice(0, 24).map(Number).filter((t) => t >= 0)) {
        const still = `${name}-${t}s.png`;
        await run("ffmpeg", ["-v", "error", "-y", "-ss", String(t), "-i", outFile, "-frames:v", "1", path.join(job.out, still)]);
        if (existsSync(path.join(job.out, still))) answer.stills.push(still);
      }
      if (answer.stills.length > 1) {
        const ins = answer.stills.flatMap((s) => ["-i", path.join(job.out, s)]);
        const scale = answer.stills.map((_, i) => `[${i}]scale=360:-2[s${i}];`).join("");
        const stack = answer.stills.map((_, i) => `[s${i}]`).join("") + `hstack=inputs=${answer.stills.length}`;
        await run("ffmpeg", ["-v", "error", "-y", ...ins, "-filter_complex", scale + stack, path.join(job.out, `${name}-contact.png`)]);
        answer.contact = `${name}-contact.png`;
      }
    }
    return answer;
  } finally {
    await fs.rm(serveUrl, { recursive: true, force: true }).catch(() => {});
  }
}

let busy = Promise.resolve();
const server = http.createServer(async (req, res) => {
  const send = (code, body) => {
    res.writeHead(code, { "content-type": code === 200 ? "application/json" : "text/plain" });
    res.end(code === 200 ? JSON.stringify(body) : String(body));
  };
  if (req.method === "GET" && req.url === "/health") return ready ? send(200, { ok: true }) : send(503, "starting");
  if (req.method !== "POST" || req.url !== "/run") return send(404, "not found");
  let raw = "";
  for await (const chunk of req) {
    raw += chunk;
    if (raw.length > 1 << 20) return send(413, "body over 1 MB");
  }
  // One render at a time: each already uses the CPUs it is given.
  const mine = busy.then(() => render(JSON.parse(raw)));
  busy = mine.catch(() => {});
  try {
    send(200, await mine);
  } catch (e) {
    send(e.code || 500, String(e.message || e));
  }
});

await ensureBrowser(); // installed in the image; this only checks
ready = true;
server.listen(PORT, () => console.log(`video: listening on :${PORT}`));
