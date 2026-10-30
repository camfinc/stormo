# Capabilities

Heavy or shared work the core runs for agents: transcribing audio, driving a browser, rendering
video, calling a paid API. An instance **declares** capabilities in `stormo.yaml capabilities:`,
**grants** them per agent in `agent.yaml capabilities: [name…]` (`stormo check` refuses an
undeclared one), and the core **brokers** every job: it checks the grant, queues, keeps each
capability to its concurrency, hands files over, meters and audits. The work runs in a worker,
never inside the core or the agent, so an agent with a grant gets the power of a heavy image
without carrying it, and its credential (if any) stays with the core or the runner.

## Kinds of worker

| kind | runs | idle cost |
|---|---|---|
| `http` | the core POSTs `{job, agent, input}` to `url`, with headers whose `${NAME}` are the core's secrets | none here |
| `container` | `docker run --rm -i` per job: the input on stdin, the answer on stdout | none |
| `service` | a long-lived container, **booted on the first job and put to sleep** (stopped and removed) after `idle_minutes` without jobs | none while asleep |

A `service` is the kind for workers with a slow start: a speech model to load, a browser to
launch, a renderer to warm up. It stays awake while jobs keep coming, sleeps when they stop, and
boots again on the next one. Its `/cache` volume (models, downloads) outlives the sleep, so only
the first boot ever downloads. The owner can wake one ahead of a batch or put one to sleep:
`stormo capability wake|sleep <name>` (the core's `POST /api/capabilities/<name>/wake|sleep`).

`container` and `service` run on this host (`placement: local`, the default) or on a runner
placement (`stormo runner <placement>` on a host with the hardware, docs/remote-core.md), which
keeps its own services and fills secrets from its own secrets file.

## Templates

The engine bundles generic workers; an instance uses one by name and overrides what it wants:

```yaml
capabilities:
  audio.transcribe: {template: transcribe}
  web.browse:       {template: browser, concurrency: 3}
  media.tools:      {template: media}
  video.render:     {template: video, idle_minutes: 5, memory_mb: 8192}
```

| template | does | files | worker |
|---|---|---|---|
| `transcribe` | speech to text with timed segments; `.txt/.srt/.vtt/.json` | audio or video in, transcripts out | whisper.cpp v1.9.5 server, CPU; model `large-v3-turbo-q5_0` (env `WHISPER_MODEL`), fetched into `/cache` on the first boot |
| `browser` | headless Chromium: open, click, fill, read text/links/HTML, eval, screenshots, PDFs; sessions per agent; **render mode** (`page`): an HTML page of a handed-over folder to an exact-size PNG, JPEG or PDF, served from that folder alone | a page folder in; screenshots, renders and PDFs out | Playwright 1.64.1; every connection goes through a proxy in the worker that refuses private, loopback and link-local addresses (redirects and DNS answers included) unless `BROWSER_ALLOW_PRIVATE: "1"` |
| `hyperframes` | renders a HyperFrames project (HTML + CSS + GSAP) to MP4/WebM/MOV/GIF; lint, validate, inspect, compositions | the project folder in, the video out | HyperFrames 0.8.97 on Playwright's image (headless shell, software GL) + ffmpeg; telemetry and update checks off; publish, auth, upgrade and preview never run |
| `media` | ffmpeg on one file: `probe`, `qa` (loudness and stills), `stills`, `loudness`, `master` (two-pass loudnorm, video copied), `cover_candidates` (the strongest moments as a labelled contact sheet: scene cuts and samples scored for sharpness, contrast and exposure, black and look-alike frames left out) and `cover` (a moment fitted to story/portrait/square/landscape, cropped or over a blurred copy) | the file in, stills, sheets, covers and masters out | ffmpeg (Debian bookworm) behind a stdlib Go adapter |
| `video` | renders a Remotion project to MP4/WebM/ProRes/GIF; optional loudness master, stills and contact sheet | the project folder in, the video and stills out | Remotion 4.0.535 with Chrome Headless Shell and ffmpeg. Remotion needs a company licence for companies of more than 3 people (remotion.dev/license) |

`stormo capability templates` lists them; `stormo capability build <template>` builds one's image
ahead of time (the core builds it on the first job otherwise: minutes, once). A template's image is
tagged `stormo-cap-<template>:<hash of its files>`, so a new engine with a changed template builds
anew and an unchanged one reuses the image. Template sources live in
`pkg/capability/templates/<name>/`: a Dockerfile, the worker, `template.yaml` (defaults, input
schema, examples) and `docs.md` (what agents read). JavaScript dependencies are installed with bun
from a committed lockfile under `minimumReleaseAge` (two weeks).

Speech models move fast. As of October 2026 the most accurate open models on the Open ASR
leaderboard (Qwen3-ASR, Cohere Transcribe, Granite Speech) need a GPU stack; the template keeps
Whisper on CPU for a worker that sleeps cheaply. Point a capability at another image (`kind:
service`, `image:`) that speaks the contract below to use one of those on a GPU runner.

## The worker contract (service)

- Listen on `$PORT` (the capability's `port`, default 8080).
- `GET <health>` (default `/health`) answers 200 once ready (model loaded, browser up). The core
  waits up to `boot_minutes` for it.
- `POST /run` takes `{job, agent, input, files, out}`: `input` is the agent's JSON; `files` are the
  handed-over inputs as paths inside the container (`/jobs/<job>/in/<name>`, files or folders);
  `out` is the folder to write files to (`/jobs/<job>/out`, "" when the capability writes none).
  Answer 200 with the job's JSON (up to 1 MB); any other status fails the job with the body as the
  reason. A request whose client goes away was cancelled.
- It runs with no Linux capabilities, no new privileges, as the user running the core (images
  must work as any user: `HOME=/tmp`, `/cache` writable by all), its port published on loopback
  only, with `memory_mb`, `shm_mb` and `cpus` as limits. Settings come from `env:` (plain values) and
  `secrets:` (names, values from the secrets file), both through the environment, never argv.
- A one-shot `container` with files gets the same folders at `/job/in` and `/job/out`.
- A job's `timeout_minutes` count from when its worker is ready: building a template's image and
  booting a sleeping worker (`boot_minutes`) come on top.

## Files

A capability with `files: in | out | both` takes shared paths and gives files back, only for
workers on this host (the core hands them over through its disk). The agent names inputs as
`files: ["/shared/<layer>/…"]`, only in its own layers (group and its unit), as everywhere else;
folders are copied without `node_modules` and `.git`, links are left out (up to 20 inputs, 4 GB).
What the worker writes to `out` lands in the agent's `out` folder, by default
`/shared/<unit>/capabilities/<capability>/job-<n>/`, recorded in the workdir's history as written
by the agent through `capability:<name>`; a file under another agent's hard or temporary lock is
not overwritten. The job's own folder (`.swarm/capabilities/jobs/`) is gone once delivered.

**Files an agent was sent** (a video in a chat) never need the shared space: `cap_start` with
`files_pending: n` creates the job **waiting**, and the agent uploads each file to
`POST /v1/capabilities/jobs/<id>/files?name=<file name>` with its core key (needletail's
`cap_attach` does it for an upload; a script can with curl). Each streams into the job's own `in/`
(the same 4 GB bound), the last one queues the job, and the folder goes with the job. A job whose
files do not all arrive within 15 minutes fails; a core restart fails waiting jobs. The worker gets
uploads after the shared inputs in `files`.

## What agents call

The core's MCP tools (every engine; needletail's Luau reaches them with `core_call` and a
`core:<tool>` grant):

| tool | |
|---|---|
| `cap_list` | the agent's capabilities: name, description, files, whether the worker is awake |
| `cap_describe {capability}` | docs, input schema, examples, limits, queue, how to call it. Never wakes the worker |
| `cap_start {capability, input, files, out, notify, files_pending}` | queue a job; `notify` (default on) leaves a notice that wakes the agent when it ends; `files_pending` makes it wait for uploads |
| `cap_run {capability, input, files, out, wait_seconds}` | start and wait up to 45 s: the answer if it ends in time, else the running job with its notice on |
| `cap_status {job}` | waiting (for uploads), queued, running, done with the answer and files, failed with why |
| `cap_jobs {limit}` | the agent's latest jobs |
| `cap_cancel {job}` | a queued job never runs; a running one is stopped |

The **notice** is a bus message from `<core>` (`[CORE NOTICE]` when it wakes the agent), with the
job's files attached; the wake loop delivers it like any message (busy agents are woken when idle,
the hourly wake cap holds). `cap_describe`, `cap_list`, `cap_status` and `cap_jobs` are read-only for
needletail's quarantine; starting, running and cancelling are writes.

## The owner

- `stormo capability list` (`GET /api/capabilities`): each capability with its worker state,
  queue, grants, and this month's spend per agent; `stormo capability jobs` the latest jobs with
  scrubbed previews. One job (`GET /api/capabilities/jobs/<id>`) shows its output too, through the
  same scrubber, cut at 64 KB; `POST …/cancel` stops any job (its agent gets the notice).
- `stormo capability show|set|remove` declare, change and remove capabilities in stormo.yaml (a JSON
  merge patch per capability, the rest of the file kept as written, checked whole before it is
  written); grants are `config apply` on an agent.yaml (`{"capabilities": [...]}`), refused for a
  capability not declared or not runnable. The core loads a change at its next restart. The macOS
  app does all of this from its Capabilities page and the agent editor.
- `stormo capability try <name> [input] [file…]` runs one job of a local container or service
  here without the core, from local files, and puts the worker back to sleep: the way to see a
  worker answer before granting it.
- Jobs that were running when the core stopped fail; the core puts its services to sleep when it
  stops and removes ones a crashed core left behind when it starts. Jobs are kept 30 days.
