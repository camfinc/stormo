// The review page: what the dream proposed for each agent, what the review's first pass said, and
// the owner's decisions. It holds no data of its own: the owner token comes in the URL fragment
// (`stormo core review` opens it), is kept for this tab only and sent as a bearer to /api/review.
// Lessons accept, reject or promote; skills, schedules and settings accept or reject; a running
// agent restarts so what was accepted applies.

const $ = (s) => document.querySelector(s);
const app = $("#app");
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

let token = "";
try {
  const m = location.hash.match(/(?:^#|&)t=([0-9a-f]{32,})/);
  if (m) sessionStorage.setItem("stormo-owner", m[1]);
  token = sessionStorage.getItem("stormo-owner") ?? "";
} catch {
  /* storage blocked: the fragment still works for this load */
  token = location.hash.match(/t=([0-9a-f]{32,})/)?.[1] ?? "";
}
if (location.hash) history.replaceState(null, "", location.pathname);

let view = null;
let busy = false;
const learningViews = new Map();
const feedbackOrigins = new Map();

function toast(text, bad = false) {
  const m = $("#msg");
  m.textContent = text;
  m.className = bad ? "msg bad" : "msg";
  m.hidden = false;
  clearTimeout(toast.t);
  toast.t = setTimeout(() => (m.hidden = true), 5000);
}

async function api(method, path, body) {
  const r = await fetch(path, {
    method,
    cache: "no-store",
    headers: { Authorization: `Bearer ${token}`, ...(body ? { "Content-Type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data?.error?.message ?? `HTTP ${r.status}`);
  return data;
}

const ago = (iso) => {
  if (!iso) return "";
  const m = Math.round((Date.now() - Date.parse(iso)) / 60000);
  return m < 1 ? "just now" : m < 60 ? `${m}m ago` : m < 2880 ? `${Math.round(m / 60)}h ago` : `${Math.round(m / 1440)}d ago`;
};

// The review's word on a proposal: why the rules held it for you, or what its model said.
function reviewTags(x) {
  if (x.held) return `<span class="rv-tag warn" title="the rules in code hold this for a person">held: ${esc(x.held)}</span>`;
  if (x.ai && !x.ai.approve) return `<span class="rv-tag warn" title="${esc(x.ai.model)}">review passed it to you: ${esc(x.ai.reason)}</span>`;
  return "";
}

function lessonHTML(a, l) {
  const tags = [
    `<span class="rv-tag">${esc(l.kind === "user" ? "about a person" : "lesson")}</span>`,
    `<span class="rv-tag">${esc(l.scope)}</span>`,
    `<span class="rv-tag">${l.seenCount} snapshot sightings</span>`,
    `<span class="rv-tag">last snapshot ${esc(ago(l.lastSeen))}</span>`,
    l.evidence?.unknown !== false
      ? `<span class="rv-tag">outcome evidence unknown</span>`
      : `<span class="rv-tag">${esc(l.evidence.confirmations)} independent confirmations · ${esc(l.evidence.failures)} failures · ${esc(l.evidence.corrections)} corrections</span>`,
    ...l.pii.map((p) => `<span class="rv-tag warn">redacted ${esc(p)}</span>`),
    l.status === "accepted"
      ? `<span class="rv-tag ok"${l.note ? ` title="${esc(l.note)}"` : ""}>accepted${l.note?.startsWith("approved by") ? " by the review" : l.note?.includes("automatically") ? " automatically" : ""}</span>`
      : "",
    l.status === "proposed" ? reviewTags(l) : "",
  ].join("");
  const promotable = l.status === "accepted" && l.kind !== "user" && !l.pii.length;
  const acts =
    l.status === "proposed"
      ? `<button class="btn yes" data-a="${esc(a.id)}" data-l="${esc(l.id)}" data-d="accept">Accept</button>
         <button class="btn no" data-a="${esc(a.id)}" data-l="${esc(l.id)}" data-d="reject">Reject</button>`
      : promotable
        ? `${l.scope === "agent" ? `<button class="btn" data-a="${esc(a.id)}" data-l="${esc(l.id)}" data-d="promote" data-s="unit">Share with unit</button>` : ""}
           ${l.scope !== "group" ? `<button class="btn" data-a="${esc(a.id)}" data-l="${esc(l.id)}" data-d="promote" data-s="group">Share with all</button>` : ""}`
        : "";
  return `<div class="item"><div class="text">${esc(l.text)}</div><div class="acts">${acts}</div><div class="rv-tags">${tags}</div></div>`;
}

function skillHTML(a, s) {
  const files = s.files
    .map(
      (f) => `<div class="fname">${esc(f.path)} <span class="note">${f.size} bytes${f.current != null ? " · changed" : s.new ? "" : " · unchanged"}</span></div>
      ${f.text ? `<pre>${esc(f.text)}</pre>` : `<p class="note">binary or large file, not shown</p>`}
      ${f.current ? `<details><summary class="note">current version</summary><pre>${esc(f.current)}</pre></details>` : ""}`,
    )
    .join("");
  return `<div class="item">
    <div class="text"><b>${esc(s.skill)}</b> <span class="note">${s.new ? "new skill" : "change to an existing skill"}</span></div>
    <div class="acts">
      <button class="btn yes" data-a="${esc(a.id)}" data-k="${esc(s.skill)}" data-d="accept">Accept</button>
      <button class="btn no" data-a="${esc(a.id)}" data-k="${esc(s.skill)}" data-d="reject">Reject</button>
    </div>
    <div class="rv-tags">${s.pii.map((p) => `<span class="rv-tag warn">contains ${esc(p)}</span>`).join("")}<span class="rv-tag">${s.files.length} file${s.files.length === 1 ? "" : "s"}</span>${s.evidence ? outcomeEvidence(s.evidence) : ""}${reviewTags(s)}</div>
    <details class="files"><summary>Show files</summary>${files}</details>
  </div>`;
}

function schedulesHTML(a, s) {
  const jobs = s.jobs
    .map((j) => {
      const runtime = s.runtimeJobs.includes(j.id);
      return `<div class="fname">${esc(j.name || j.id)} <span class="note">${esc(j.every || j.cron)} · ${j.enabled ? "on" : "paused"}${runtime ? " · created at runtime" : ""}</span></div>
        ${j.prompt ? `<pre>${esc(j.prompt)}</pre>` : ""}`;
    })
    .join("");
  return `<div class="item">
    <div class="text"><b>${s.jobs.length} scheduled job${s.jobs.length === 1 ? "" : "s"}</b> <span class="note">as agent.yaml would hold them; reject drops the ${s.runtimeJobs.length} created at runtime</span></div>
    <div class="acts">
      <button class="btn yes" data-a="${esc(a.id)}" data-sch="1" data-d="accept">Accept</button>
      <button class="btn no" data-a="${esc(a.id)}" data-sch="1" data-d="reject">Reject</button>
    </div>
    <div class="rv-tags">${s.pii.map((p) => `<span class="rv-tag warn">redacted ${esc(p)}</span>`).join("")}${reviewTags(s)}</div>
    <details class="files"><summary>Show jobs</summary>${jobs}</details>
  </div>`;
}

function settingHTML(a, s) {
  return `<div class="item">
    <div class="text"><b>${esc(s.key)}</b> <span class="note">${s.current ? `now ${esc(s.current)}` : "not set in the repo"}</span><pre>${esc(s.value)}</pre></div>
    <div class="acts">
      <button class="btn yes" data-a="${esc(a.id)}" data-set="${esc(s.key)}" data-d="accept">Accept</button>
      <button class="btn no" data-a="${esc(a.id)}" data-set="${esc(s.key)}" data-d="reject">Reject</button>
    </div>
    <div class="rv-tags">${s.sensitive ? `<span class="rv-tag warn">widens what the agent may do</span>` : ""}${s.pii.map((p) => `<span class="rv-tag warn">redacted ${esc(p)}</span>`).join("")}${reviewTags(s)}</div>
  </div>`;
}

function agentHTML(a) {
  const proposed = a.lessons.filter((l) => l.status === "proposed");
  const accepted = a.lessons.filter((l) => l.status === "accepted");
  const pending = proposed.length + a.skills.length + (a.schedules ? 1 : 0) + a.settings.length;
  const restart = a.unapplied
    ? a.state === "running"
      ? `<button class="btn go" data-restart="${esc(a.id)}"${a.busy ? ' title="it is busy now; restarting interrupts its work"' : ""}>Restart to apply${a.busy ? " (busy)" : ""}</button>`
      : `<span class="note">applies when it next starts</span>`
    : "";
  return `<section class="rv-agent" aria-labelledby="h-${esc(a.id)}">
    <header>
      <h2 id="h-${esc(a.id)}">${esc(a.name)}</h2>
      <span class="meta">${esc(a.unit)} · ${esc(a.state)}${a.busy ? " · busy" : ""}${view.review === "auto" && !a.autoReview ? " · no automatic review" : ""}</span>
      <span class="spacer"></span>
      <span class="pill"><b>${pending}</b> to review</span>
      ${restart}
    </header>
    ${pending === 0 && accepted.length === 0 ? `<p class="empty">Nothing proposed yet. Proposals appear after a learning cycle (swarm core learn ${esc(a.id)}).</p>` : ""}
    ${proposed.length ? `<div class="sec"><h3>Proposed lessons</h3>${proposed.map((l) => lessonHTML(a, l)).join("")}</div>` : ""}
    ${a.skills.length ? `<div class="sec"><h3>Skill proposals</h3>${a.skills.map((s) => skillHTML(a, s)).join("")}</div>` : ""}
    ${a.schedules ? `<div class="sec"><h3>Schedules changed at runtime</h3>${schedulesHTML(a, a.schedules)}</div>` : ""}
    ${a.settings.length ? `<div class="sec"><h3>Settings changed at runtime</h3>${a.settings.map((s) => settingHTML(a, s)).join("")}</div>` : ""}
    ${learningHTML(a)}
    ${accepted.length ? `<details class="sec"><summary><h3 style="display:inline">Accepted lessons (${accepted.length})</h3></summary>${accepted.map((l) => lessonHTML(a, l)).join("")}</details>` : ""}
  </section>`;
}

function render() {
  if (!token) {
    app.innerHTML = `<div class="gate"><h1>Review</h1><p>This page needs the owner token. Open it from a terminal with <code>stormo core review</code>.</p></div>`;
    return;
  }
  if (!view) return;
  const total = view.agents.reduce(
    (n, a) => n + a.lessons.filter((l) => l.status === "proposed").length + a.skills.length + (a.schedules ? 1 : 0) + a.settings.length,
    0,
  );
  const auto =
    view.review === "auto"
      ? `<span class="pill on" title="${esc(view.reviewConnection)}${view.reviewModel ? " · " + esc(view.reviewModel) : ""}${view.reviewPrompt ? " · your rules: " + esc(view.reviewPrompt) : ""}">automatic review on: it approves what is obviously safe, never rejects${view.autoRestart ? ", restarts idle agents" : ""}</span>`
      : `<span class="pill">automatic review off · <code>core.learning.review</code></span>`;
  app.innerHTML = `<h1>What the agents learned</h1>
    <p class="lede">Proposals from the dream: lessons the agents wrote to their memory (already scrubbed of personal data), skills they created or changed, and schedules and settings they changed while running. What is here waits for you: the automatic review approved the rest or passed these on, and the rules hold some for a person without asking it. Accepted changes reach an agent when it restarts. Decisions edit <code>agents/*/learnings</code>, <code>skills</code> and <code>agent.yaml</code> in the working tree; committing them is yours.</p>
    <div class="bar"><span class="pill"><b>${total}</b> waiting across ${view.agents.length} agents</span>${auto}<button class="btn" id="reload">Refresh</button></div>
    ${view.agents.map(agentHTML).join("")}`;
}

async function load() {
  if (!token) return render();
  try {
    view = await api("GET", "/api/review");
    render();
    restoreLearningPanels();
  } catch (e) {
    app.innerHTML = `<div class="gate"><h1>Review</h1><p>${esc(e.message)}</p></div>`;
  }
}

app.addEventListener("click", async (ev) => {
  const b = ev.target.closest("button");
  if (!b || busy) return;
  if (b.dataset.learningRefresh) return loadLearning(b.dataset.learningRefresh);
  if (b.dataset.exportCandidate) return exportCandidate(b);
  if (b.closest(".learning")) return;
  if (b.id === "reload") return load();
  busy = true;
  b.disabled = true;
  try {
    if (b.dataset.restart) {
      toast(`Restarting ${b.dataset.restart}… (a final nap, then a fresh start)`);
      await api("POST", `/api/agents/${encodeURIComponent(b.dataset.restart)}/restart`);
      toast(`${b.dataset.restart} restarted with its accepted lessons and skills`);
    } else if (b.dataset.sch) {
      await api("POST", "/api/review", { agent: b.dataset.a, schedules: true, decision: b.dataset.d });
      toast(`Schedules ${b.dataset.d === "accept" ? "accepted into agent.yaml" : "rejected"}`);
    } else if (b.dataset.set) {
      await api("POST", "/api/review", { agent: b.dataset.a, settings: [b.dataset.set], decision: b.dataset.d });
      toast(`${b.dataset.set} ${b.dataset.d === "accept" ? "accepted" : "rejected"}`);
    } else if (b.dataset.k) {
      await api("POST", "/api/review", { agent: b.dataset.a, skill: b.dataset.k, decision: b.dataset.d });
      toast(`Skill ${b.dataset.k} ${b.dataset.d === "accept" ? "accepted" : "rejected"}`);
    } else if (b.dataset.l) {
      await api("POST", "/api/review", { agent: b.dataset.a, lessons: [b.dataset.l], decision: b.dataset.d, scope: b.dataset.s });
      toast(b.dataset.d === "promote" ? `Shared with ${b.dataset.s === "unit" ? "the unit" : "every agent"}` : `Lesson ${b.dataset.d}ed`);
    }
    await load();
  } catch (e) {
    toast(e.message, true);
    b.disabled = false;
  } finally {
    busy = false;
  }
});

load();
setInterval(() => !busy && !document.querySelector(".learning[open]") && document.visibilityState === "visible" && load(), 30000);

function learningHTML(a) {
  return `<details class="sec learning" data-learning-agent="${esc(a.id)}"><summary>Task outcomes and reflection</summary><div class="learning-body"><p class="notice">Open to review outcomes and proposed procedures for a conversation.</p></div></details>`;
}

function learningPath(id, session = "") {
  return `/api/agents/${encodeURIComponent(id)}/learning${session ? `?session=${encodeURIComponent(session)}` : ""}`;
}

function learningPanel(id) {
  return [...app.querySelectorAll("[data-learning-agent]")].find((p) => p.dataset.learningAgent === id);
}

function outcomeEvidence(e) {
  if (!e) return `<span class="rv-tag">outcome evidence unknown</span>`;
  return `<span class="rv-tag">${esc(e.confirmations ?? 0)} independent confirmations · ${esc(e.failures ?? 0)} failures · ${esc(e.corrections ?? 0)} corrections</span>${e.unknown ? `<span class="rv-tag">outcome evidence unknown</span>` : ""}`;
}

function eventHTML(id, e, status) {
  const canFeedback = status.feedbackAllowed === true && ["runtime", "view"].includes(e.sourceType);
  const subject = `${e.subjectKind} ${String(e.subjectId).slice(0, 12)}`;
  return `<div class="item"><div class="text"><b>${esc(subject)}</b> <span class="note">${esc(ago(e.at))}</span></div>
    <div class="rv-tags"><span class="rv-tag">${esc(e.sourceType)}</span><span class="rv-tag">execution ${esc(e.executionStatus)}</span><span class="rv-tag${e.taskOutcome === "failed" || e.taskOutcome === "corrected" ? " warn" : ""}">task ${esc(e.taskOutcome)}</span><span class="rv-tag">${esc(e.audience === "agent" ? "agent-wide" : "conversation audience")}</span></div>
    ${canFeedback ? `<details class="outcome-form"><summary>Record a checked outcome</summary><form data-feedback-agent="${esc(id)}" data-event-id="${esc(e.eventId)}">
      <label>Task result<select name="outcome" required><option value="">Choose a result…</option><option value="succeeded">Succeeded — the task check passed</option><option value="failed">Failed — the task check failed</option><option value="corrected">Corrected — the claim or procedure changed</option></select></label>
      <label>Checked result or correction <span class="note">Optional. Up to 1,000 characters and 2,000 UTF-8 bytes. Include only information this conversation may use.</span><textarea name="summary" maxlength="1000"></textarea></label>
      <button class="btn" type="submit">Save outcome</button><p class="note">A successful command alone does not confirm task success. Saving feedback may create a reflection proposal when reflection is enabled.</p>
    </form></details>` : ""}</div>`;
}

function candidateHTML(id, c, status) {
  const exported = (status.exportedIds ?? []).includes(c.id);
  const publicCandidate = c.audience === "agent" && ["public", "procedural"].includes(c.sensitivity);
  const allowed = status.exportAllowed === true && publicCandidate && !exported;
  return `<div class="item"><div class="text">${esc(c.text)}</div><div class="acts"><button class="btn" data-export-candidate="${esc(c.id)}" data-learning-id="${esc(id)}" ${allowed ? "" : "disabled"}>${exported ? "Sent for review" : "Send for review"}</button></div>
    <div class="rv-tags"><span class="rv-tag">${esc(c.action)} proposal</span><span class="rv-tag">${esc(c.audience === "agent" ? "agent-wide" : "private audience")}</span><span class="rv-tag">review by ${esc(c.reviewDate)}</span></div>
    <div class="outcome-form"><p class="detail-copy"><b>Applies:</b> ${esc(c.conditions)}</p>${c.exceptions ? `<p class="detail-copy"><b>Exceptions:</b> ${esc(c.exceptions)}</p>` : ""}<p class="detail-copy"><b>Supporting checks:</b> ${esc(c.checks)}</p>${c.previousMistake ? `<p class="detail-copy"><b>Correction:</b> ${esc(c.previousMistake)}</p>` : ""}<p class="note">${exported ? "Pending the learning cycle and review. This procedure is not active knowledge." : !publicCandidate ? "Private or sensitive proposals stay in this conversation and cannot be sent for shared review." : "Sending selects this procedure for the next learning cycle and review. It does not activate it or change a live skill."}</p></div></div>`;
}

function renderLearning(id) {
  const panel = learningPanel(id), state = learningViews.get(id);
  if (!panel || !state) return;
  panel.open = state.open;
  const body = panel.querySelector(".learning-body");
  body.setAttribute("aria-busy", String(state.loading || state.mutating));
  const envelope = state.envelope ?? {}, data = envelope.data, status = { ...(data?.status ?? {}), ...(state.loading || state.mutating ? { feedbackAllowed: false, exportAllowed: false } : {}) };
  const sessions = envelope.sessions ?? [];
  const options = sessions.map((s) => `<option value="${esc(s.id)}"${s.id === state.session ? " selected" : ""}>${esc(s.title || s.preview || s.id)}${s.source ? ` · ${esc(s.source)}` : ""}</option>`).join("");
  const controls = `<div class="learning-controls"><label>Conversation<select data-learning-session="${esc(id)}" ${state.loading || state.mutating || !sessions.length ? "disabled" : ""}><option value="">Choose a conversation…</option>${options}</select></label><button class="btn" data-learning-refresh="${esc(id)}" ${state.loading || state.mutating ? "disabled" : ""}>${state.loading ? "Loading…" : "Refresh outcomes"}</button></div>`;
  const notice = state.error ? `<p class="notice error" role="alert">${esc(state.error)} Refresh outcomes to try again.</p>` : envelope.state && envelope.state !== "ready" ? `<p class="notice" role="status">${esc(envelope.message || "Learning review is unavailable for this agent.")}</p>` : "";
  body.innerHTML = `<p class="note">Review checked task outcomes and reflection proposals. Proposals remain pending until the learning cycle and review accept them.</p>${controls}${notice}
    ${data ? `<div class="bar"><span class="pill">Reflection <b>${status.reflectionEnabled ? "enabled" : "disabled"}</b></span><span class="pill">Calls <b>${esc(status.callsUsed ?? 0)} / ${esc(status.callsLimit ?? 100)}</b></span><span class="pill">Events ${esc(status.eventsUsed ?? 0)} / ${esc(status.eventsLimit ?? 4096)}</span><span class="pill">Candidates ${esc(status.candidatesUsed ?? 0)} / ${esc(status.candidatesLimit ?? 100)}</span><span class="pill">Exports ${esc(status.exportsUsed ?? 0)} / ${esc(status.exportsLimit ?? 100)}</span></div>
      ${!status.reflectionEnabled ? `<p class="note">Outcome feedback is still recorded. Reflection is disabled, so it creates no new reflection jobs.</p>` : ""}${status.feedbackAllowed === false ? `<p class="notice">Feedback is unavailable for this conversation. Existing outcomes and proposals remain readable.</p>` : ""}
      <h4>Observed outcomes (${(data.events ?? []).length})</h4>${(data.events ?? []).length ? data.events.map((e) => eventHTML(id, e, status)).join("") : `<p class="notice">No attributed outcomes in this conversation yet. Views and command exits alone do not establish task success.</p>`}
      <h4>Reflection proposals (${(data.candidates ?? []).length})</h4>${(data.candidates ?? []).length ? data.candidates.map((c) => candidateHTML(id, c, status)).join("") : `<p class="notice">No reflection proposals for this conversation. A routine turn may produce none.</p>`}` : !state.loading && !state.error && envelope.state === "ready" ? `<p class="notice">${sessions.length ? "Choose a conversation to see its outcomes and proposals." : "No conversations available for this agent yet."}</p>` : ""}`;
}

async function loadLearning(id, session) {
  const state = learningViews.get(id) ?? { open: true, session: "", envelope: null };
  if (state.loading || state.mutating || busy) return;
  if (session !== undefined && session !== state.session) {
    state.session = session;
    if (state.envelope) state.envelope = { ...state.envelope, data: undefined };
  }
  state.loading = true;
  state.error = "";
  learningViews.set(id, state);
  renderLearning(id);
  try { state.envelope = await api("GET", learningPath(id, state.session)); }
  catch (e) { state.error = e.message; }
  finally { state.loading = false; renderLearning(id); }
}

function restoreLearningPanels() {
  for (const [id] of learningViews) renderLearning(id);
}

app.addEventListener("toggle", (ev) => {
  const panel = ev.target;
  if (!panel.matches?.("[data-learning-agent]")) return;
  const id = panel.dataset.learningAgent;
  const state = learningViews.get(id);
  if (state) state.open = panel.open;
  else if (panel.open) loadLearning(id);
}, true);

app.addEventListener("change", (ev) => {
  if (ev.target.dataset.learningSession) loadLearning(ev.target.dataset.learningSession, ev.target.value);
});

app.addEventListener("submit", async (ev) => {
  const form = ev.target.closest("[data-feedback-agent]");
  if (!form) return;
  ev.preventDefault();
  if (busy || !form.reportValidity()) return;
  const id = form.dataset.feedbackAgent, state = learningViews.get(id);
  if (state.loading || state.mutating) return;
  const session = state.session;
  const outcome = form.elements.outcome.value;
  const key = `${id}:${state.session}:${form.dataset.eventId}:${outcome}`;
  if (!feedbackOrigins.has(key)) feedbackOrigins.set(key, Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, "0")).join(""));
  const payload = { eventId: form.dataset.eventId, outcome, origin: feedbackOrigins.get(key), summary: form.elements.summary.value };
  if (new TextEncoder().encode(payload.summary).length > 2000) return toast("The result summary exceeds 2,000 UTF-8 bytes. Shorten it and save again.", true);
  if (new TextEncoder().encode(JSON.stringify(payload)).length > 4096) return toast("The result summary is too long. Shorten it and save again.", true);
  busy = true;
  state.mutating = true;
  lockLearningControls(id, true);
  const button = form.querySelector("button");
  button.disabled = true; button.textContent = "Saving…";
  try {
    state.envelope = await api("POST", learningPath(id, session), payload);
    if (state.envelope.state !== "ready") throw new Error(state.envelope.message || "Outcome could not be saved.");
    renderLearning(id);
    const operation = state.envelope.data?.operation;
    toast(operation?.ok === false ? operation.message || "Feedback was saved, but reflection could not finish. Review the recorded outcome before trying again." : "Outcome saved. Any reflection proposal still needs review.", operation?.ok === false);
  } catch (e) { toast(e.message, true); button.disabled = false; button.textContent = "Save outcome"; }
  finally { busy = false; state.mutating = false; renderLearning(id); }
});

async function exportCandidate(button) {
  const id = button.dataset.learningId, state = learningViews.get(id);
  if (state.loading || state.mutating) return;
  const session = state.session;
  busy = true; state.mutating = true; lockLearningControls(id, true);
  button.disabled = true; button.textContent = "Sending…";
  try {
    state.envelope = await api("POST", learningPath(id, session), { exportId: button.dataset.exportCandidate });
    if (state.envelope.state !== "ready") throw new Error(state.envelope.message || "Proposal could not be sent.");
    renderLearning(id);
    const operation = state.envelope.data?.operation;
    toast(operation?.ok === false ? operation.message || "Proposal could not be sent. Refresh to check its status." : "Selected for the next learning cycle and review. It is not active yet.", operation?.ok === false);
  } catch (e) { toast(e.message, true); button.disabled = false; button.textContent = "Send for review"; }
  finally { busy = false; state.mutating = false; renderLearning(id); }
}

// Keep the selected conversation fixed while its outcome/export mutation is in flight.
function lockLearningControls(id, locked) {
  const panel = learningPanel(id);
  panel?.querySelectorAll("select, textarea, button").forEach((control) => { control.disabled = locked; });
  panel?.querySelector(".learning-body").setAttribute("aria-busy", String(locked));
}
