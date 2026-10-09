// The review page: what the dream proposed for each agent, and the owner's decisions. It holds no
// data of its own: the owner token comes in the URL fragment (`stormo core review` opens it), is
// kept for this tab only and sent as a bearer to /api/review. Lessons accept, reject or promote;
// skills accept or reject; a running agent restarts so what was accepted applies.

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

function lessonHTML(a, l) {
  const tags = [
    `<span class="tag">${esc(l.kind === "user" ? "about a person" : "lesson")}</span>`,
    `<span class="tag">${esc(l.scope)}</span>`,
    `<span class="tag">seen ${l.seenCount}×</span>`,
    `<span class="tag">last ${esc(ago(l.lastSeen))}</span>`,
    ...l.pii.map((p) => `<span class="tag warn">redacted ${esc(p)}</span>`),
    l.status === "accepted" ? `<span class="tag ok">accepted${l.note?.includes("automatically") ? " automatically" : ""}</span>` : "",
    l.status === "proposed" && l.autoAcceptable ? `<span class="tag">auto-acceptable</span>` : "",
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
  return `<div class="item"><div class="text">${esc(l.text)}</div><div class="acts">${acts}</div><div class="tags">${tags}</div></div>`;
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
    <div class="tags">${s.pii.map((p) => `<span class="tag warn">contains ${esc(p)}</span>`).join("")}<span class="tag">${s.files.length} file${s.files.length === 1 ? "" : "s"}</span></div>
    <details class="files"><summary>Show files</summary>${files}</details>
  </div>`;
}

function agentHTML(a) {
  const proposed = a.lessons.filter((l) => l.status === "proposed");
  const accepted = a.lessons.filter((l) => l.status === "accepted");
  const pending = proposed.length + a.skills.length;
  const restart = a.unapplied
    ? a.state === "running"
      ? `<button class="btn go" data-restart="${esc(a.id)}"${a.busy ? ' title="it is busy now; restarting interrupts its work"' : ""}>Restart to apply${a.busy ? " (busy)" : ""}</button>`
      : `<span class="note">applies when it next starts</span>`
    : "";
  return `<section class="agent" aria-labelledby="h-${esc(a.id)}">
    <header>
      <h2 id="h-${esc(a.id)}">${esc(a.name)}</h2>
      <span class="meta">${esc(a.unit)} · ${esc(a.state)}${a.busy ? " · busy" : ""}</span>
      <span class="spacer"></span>
      <span class="pill"><b>${pending}</b> to review</span>
      ${proposed.some((l) => l.autoAcceptable) ? `<button class="btn" data-bulk="${esc(a.id)}">Accept the ${proposed.filter((l) => l.autoAcceptable).length} auto-acceptable</button>` : ""}
      ${restart}
    </header>
    ${pending === 0 && accepted.length === 0 ? `<p class="empty">Nothing proposed yet. Proposals appear after a learning cycle (swarm core learn ${esc(a.id)}).</p>` : ""}
    ${proposed.length ? `<div class="sec"><h3>Proposed lessons</h3>${proposed.map((l) => lessonHTML(a, l)).join("")}</div>` : ""}
    ${a.skills.length ? `<div class="sec"><h3>Skill proposals</h3>${a.skills.map((s) => skillHTML(a, s)).join("")}</div>` : ""}
    ${accepted.length ? `<details class="sec"><summary><h3 style="display:inline">Accepted lessons (${accepted.length})</h3></summary>${accepted.map((l) => lessonHTML(a, l)).join("")}</details>` : ""}
  </section>`;
}

function render() {
  if (!token) {
    app.innerHTML = `<div class="gate"><h1>Review</h1><p>This page needs the owner token. Open it from a terminal with <code>stormo core review</code>.</p></div>`;
    return;
  }
  if (!view) return;
  const total = view.agents.reduce((n, a) => n + a.lessons.filter((l) => l.status === "proposed").length + a.skills.length, 0);
  const auto = view.autoAccept
    ? `<span class="pill on">auto-accept on${view.autoAcceptMinSeen > 1 ? ` (seen ${view.autoAcceptMinSeen}×+)` : ""}${view.autoRestart ? ", restarts idle agents" : ""}</span>`
    : `<span class="pill">auto-accept off · <code>core.learning.auto_accept</code></span>`;
  app.innerHTML = `<h1>What the agents learned</h1>
    <p class="lede">Proposals from the dream: lessons the agents wrote to their memory (already scrubbed of personal data) and skills they created or changed. Accepted lessons and skills reach an agent when it restarts. Decisions edit <code>agents/*/learnings</code> and <code>skills</code> in the working tree; committing them is yours.</p>
    <div class="bar"><span class="pill"><b>${total}</b> waiting across ${view.agents.length} agents</span>${auto}<button class="btn" id="reload">Refresh</button></div>
    ${view.agents.map(agentHTML).join("")}`;
}

async function load() {
  if (!token) return render();
  try {
    view = await api("GET", "/api/review");
    render();
  } catch (e) {
    app.innerHTML = `<div class="gate"><h1>Review</h1><p>${esc(e.message)}</p></div>`;
  }
}

app.addEventListener("click", async (ev) => {
  const b = ev.target.closest("button");
  if (!b || busy) return;
  if (b.id === "reload") return load();
  busy = true;
  b.disabled = true;
  try {
    if (b.dataset.restart) {
      toast(`Restarting ${b.dataset.restart}… (a final nap, then a fresh start)`);
      await api("POST", `/api/agents/${encodeURIComponent(b.dataset.restart)}/restart`);
      toast(`${b.dataset.restart} restarted with its accepted lessons and skills`);
    } else if (b.dataset.bulk) {
      const a = view.agents.find((x) => x.id === b.dataset.bulk);
      const ids = a.lessons.filter((l) => l.status === "proposed" && l.autoAcceptable).map((l) => l.id);
      await api("POST", "/api/review", { agent: a.id, lessons: ids, decision: "accept" });
      toast(`Accepted ${ids.length} lessons for ${a.name}`);
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
setInterval(() => !busy && document.visibilityState === "visible" && load(), 30000);
