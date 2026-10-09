// Stormo HQ: renders /api/fleet as a top-down office floor. Units are offices off two hallways;
// the middle of the floor is the server room (where the swarm core runs) above the lobby, with the
// main entrance and the kitchen. Each agent has its own desk, dressed from its persona's `desk:`
// block (personas/README.md) or its unit's defaults, and walks in from the entrance when it starts.
// The page polls the core (which serves a cached snapshot); it is read-only: lifecycle commands are
// shown to copy and run in a terminal.

const POLL_MS = 2500;
const MIN_DESKS = 4;
const PRESENT = new Set(["idle", "busy", "arriving", "alert", "unknown"]);
const MODE = {
  idle: { label: "At desk", color: "var(--ok)" },
  busy: { label: "Working", color: "var(--busy)" },
  arriving: { label: "Arriving", color: "var(--warn)" },
  alert: { label: "Needs attention", color: "var(--bad)" },
  unknown: { label: "Unknown", color: "var(--off)" },
  away: { label: "Off shift", color: "var(--off)" },
  leaving: { label: "Leaving", color: "var(--off)" },
};

// What a desk holds when neither the persona nor the unit (stormo.yaml office.units) says.
const DEFAULT_DESK = { app: "chat", screens: 1, props: ["notes", "mug", "plant"], side: "bin" };
const APPS = new Set(["records", "inbox", "code", "design", "charts", "chat"]);
const PROPS = new Set([
  "folders", "ticket", "id-card", "headset", "phone", "globe", "stamp", "laptop", "duck", "notes", "mug",
  "tablet", "camera", "swatches", "books", "chart", "magnifier", "notebook", "lamp", "plant", "photo",
]);
const SIDES = new Set(["suitcase", "plant", "bin", "ring-light", "shelf", "whiteboard"]);
// The instance (stormo.yaml), injected by the core into index.html.
const INSTANCE = window.STORMO ?? { name: "Stormo", org: "", clocks: [], units: {} };
// Per-unit look: stormo.yaml office.units.<unit> {hue, wall, floor, desk}.
const UNITS = INSTANCE.units ?? {};
// The world clocks on the lobby wall: stormo.yaml office.clocks.
const CLOCKS = (INSTANCE.clocks ?? []).map((c) => ({ label: c.city, tz: c.tz }));
const clockFormats = CLOCKS.map((c) => ({
  ...c,
  hm: new Intl.DateTimeFormat("en-GB", { timeZone: c.tz, hour: "2-digit", minute: "2-digit", hourCycle: "h23" }),
  full: new Intl.DateTimeFormat("en-GB", { timeZone: c.tz, weekday: "long", day: "numeric", month: "long", hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZoneName: "short" }),
}));
// What hangs on an office's back wall, and its floor, when the unit does not say.
const WALL_ART = ["map", "board", "moodboard", "shelves"];
const FLOORS = ["wood", "carpet", "concrete", "library"];
const PALETTE = ["#2dd4bf", "#818cf8", "#f472b6", "#fbbf24", "#60a5fa", "#fb923c", "#a3e635", "#e879f9"];
const GROUP_HUE = "#34d399";

const $ = (s, el = document) => el.querySelector(s);
const floor = $("#floor");
const links = $("#links");
const inspector = $("#inspector");
const body = $("#inspector-body");
const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)");

let snap = null;
let rosterKey = "";
let selected = null; // {kind: "agent"|"core"|"vacant", id?, unit?}
const prevMode = new Map();
const prevReview = new Map();
const lastBusyLog = new Map();
let prevGateway = null;
let firstPaint = true;
let failures = 0;

// ---------- helpers ----------

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

function hash(s) {
  let h = 2166136261;
  for (const c of s) h = Math.imul(h ^ c.charCodeAt(0), 16777619);
  return h >>> 0;
}

const hueOf = (unit) => UNITS[unit]?.hue ?? (unit === "group" ? GROUP_HUE : PALETTE[hash(unit) % PALETTE.length]);
const wallOf = (unit) => (WALL_ART.includes(UNITS[unit]?.wall) ? UNITS[unit].wall : WALL_ART[hash(unit) % WALL_ART.length]);
const floorOf = (unit) => (FLOORS.includes(UNITS[unit]?.floor) ? UNITS[unit].floor : FLOORS[hash(unit) % FLOORS.length]);

function ago(iso) {
  if (!iso) return "never";
  const s = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  return m < 60 ? `${m}m ago` : m < 2880 ? `${Math.round(m / 60)}h ago` : `${Math.round(m / 1440)}d ago`;
}

const compact = (n) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${(n / 1e3).toFixed(n >= 1e4 ? 0 : 1)}k` : String(n ?? 0));
const clockTime = (d = new Date()) => d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
const initials = (name) => name.split(/\s+/).map((w) => w[0]).join("").slice(0, 2).toUpperCase();
const ORG_PREFIX = INSTANCE.org ? new RegExp(`^${INSTANCE.org.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\s+`, "i") : null;
const shortName = (name) => (ORG_PREFIX ? name.replace(ORG_PREFIX, "") : name);

// Working: the core decides (office.working: an engine turn or a model call in flight, held across
// tool gaps), so every viewer counts the same. The fallback below is for a core without it.
const HOLD_MS = 8000;
const lastWorkAt = new Map();
const workingNow = (a, gw) =>
  (a.activity?.activeAgents ?? 0) > 0 || a.activity?.gatewayBusy === true || (a.activity?.runningJobs?.length ?? 0) > 0 || (gw?.active?.[a.id] ?? 0) > 0;

function modeOf(a, gw) {
  if (a.state === "running") {
    if (a.health === "unhealthy" || a.detail || Object.values(a.activity?.platforms ?? {}).some((p) => p.needsAttention)) return "alert";
    if (a.office) return a.office.working ? "busy" : "idle";
    // A core without the office simulation: decide here, with the same hold.
    if (workingNow(a, gw)) {
      lastWorkAt.set(a.id, Date.now());
      return "busy";
    }
    return Date.now() - (lastWorkAt.get(a.id) ?? 0) < HOLD_MS ? "busy" : "idle";
  }
  if (a.state === "starting" || a.state === "restarting") return "arriving";
  if (a.state === "unknown") return "unknown";
  return "away";
}

const review = (a) => a.pendingLearnings + a.pendingSkills;
const unitName = (id) => snap?.units.find((u) => u.id === id)?.name ?? id;

/** The persona's desk over its unit's defaults, reduced to what this page knows how to draw. */
function deskOf(a) {
  const base = { ...DEFAULT_DESK, ...(UNITS[a.unit]?.desk ?? {}) };
  const d = a.desk ?? {};
  const apps = (d.apps ?? (d.app ? [d.app] : [])).filter((x) => APPS.has(x));
  const app = apps[0] ?? base.app;
  const screens = d.screens ?? (apps.length > 1 ? 2 : d.app ? 1 : base.screens);
  return {
    app,
    // What each monitor shows: the persona's list, else the one app on every screen.
    apps: Array.from({ length: screens }, (_, i) => apps[i] ?? app),
    screens,
    props: (d.props?.length ? d.props : base.props).filter((p) => PROPS.has(p)).slice(0, 6),
    side: SIDES.has(d.side) ? d.side : base.side,
  };
}

// ---------- floor plan ----------

function faceHTML(a) {
  return a.avatar ? `<img src="/avatars/${esc(a.id)}.png" alt="" loading="lazy" draggable="false">` : esc(initials(a.name));
}

const screenHTML = (app) => `<div class="monitor"><div class="screen app-${app}">${"<i></i>".repeat(6)}</div></div>`;

function workstation(a, unit) {
  if (!a) {
    return `<div class="ws vacant" style="--hue:${hueOf(unit)}">
      <button class="desk-hit" data-vacant="${esc(unit)}" aria-label="Open desk in ${esc(unitName(unit))}"></button>
      <div class="desk"><div class="monitors">${screenHTML("off")}</div><div class="kbd"></div><div class="plaque">open desk</div></div>
      <div class="chair"></div></div>`;
  }
  const d = deskOf(a);
  const props = d.props.map((p, i) => `<span class="prop p-${p} s${i + 1}"></span>`).join("");
  return `<div class="ws" data-agent="${esc(a.id)}" data-mode="away" style="--hue:${hueOf(a.unit)}">
    <button class="desk-hit" data-select="${esc(a.id)}" aria-label="${esc(a.name)}'s desk"></button>
    <span class="side x-${d.side}" aria-hidden="true"></span>
    <div class="desk"><div class="monitors${d.screens === 2 ? " dual" : ""}">${d.apps.map(screenHTML).join("")}</div>
      <div class="kbd"></div><span class="mouse"></span>${props}
      <div class="plaque">${esc(a.name)}</div><div class="sticky">off shift</div></div>
    <div class="chair"></div>
    <button class="agent" data-select="${esc(a.id)}" aria-pressed="false">
      <span class="shadow"></span>${charSVG(a)}
      <span class="bubble" aria-hidden="true"><span class="src"></span> <span class="dots"><span>·</span><span>·</span><span>·</span></span></span>
      <span class="marker review" title="Waiting for review"></span>
      <span class="marker alert" aria-hidden="true">!</span>
      <span class="tag"><span class="mini">${faceHTML(a)}</span><b>${esc(a.name)}</b><span class="what"></span></span>
    </button>
  </div>`;
}

const windows = (sides) => sides.map((s) => `<span class="win ${s}" aria-hidden="true"></span>`).join("");

function officeHTML(u, agents, side, row, span, rows) {
  const desks = agents.map((a) => workstation(a, u.id));
  const total = Math.max(MIN_DESKS, agents.length + (agents.length % 2));
  for (let i = agents.length; i < total; i++) desks.push(workstation(null, u.id));
  const outer = [side, ...(row === 1 ? ["top"] : []), ...(row + span - 1 === rows ? ["bottom"] : [])];
  return `<section class="room ${side}" data-unit="${esc(u.id)}" data-floor="${floorOf(u.id)}" style="--hue:${hueOf(u.id)};grid-row:${row} / span ${span}">
    <div class="sign"><span class="sign-name">${esc(shortName(u.name))}</span><span class="sign-desc" title="${esc(u.description)}">${esc(u.description)}</span></div>
    ${windows(outer)}
    <span class="door ${side === "left" ? "right" : "left"}" aria-hidden="true"></span>
    <span class="wall-art w-${wallOf(u.id)}" aria-hidden="true"></span>
    <span class="plant corner" aria-hidden="true"></span>
    <div class="desks">${desks.join("")}</div>
  </section>`;
}

const rackUnits = (n, kind) => Array.from({ length: n }, () => `<div class="unit ${kind}">${"<i></i>".repeat(4)}<b></b></div>`).join("");

function serverRoomHTML(group) {
  return `<section class="server" data-unit="group" aria-label="Server room">
    <div class="sign"><span class="sign-name">Server room</span><span class="sign-desc">${esc(shortName(group.name))} · authorised staff only</span></div>
    <span class="glass left" aria-hidden="true"></span><span class="glass right" aria-hidden="true"></span>
    <span class="door bottom" aria-hidden="true"><span class="badge-reader"></span></span>
    <span class="tray" aria-hidden="true"></span>
    <div class="racks">
      <div class="rack decor" aria-hidden="true">${rackUnits(7, "net")}<span class="label">net</span></div>
      <button class="rack core" id="rack" aria-pressed="false" aria-label="Swarm core">
        ${rackUnits(6, "srv")}
        <div class="slots" id="slots"></div>
        <span class="label">Swarm core</span><span class="sub" id="rack-sub">…</span>
      </button>
      <div class="rack fleet" aria-label="Agent containers"><div id="blades"></div><span class="label">agents</span></div>
      <div class="rack decor" aria-hidden="true">${rackUnits(5, "ups")}<span class="label">ups</span></div>
    </div>
    <span class="crac" aria-hidden="true"><span class="fan"></span></span>
    <span class="extinguisher" aria-hidden="true"></span>
  </section>`;
}

function lobbyHTML(commonsAgents) {
  return `<section class="lobby" data-unit="group" aria-label="Lobby and kitchen">
    <div class="sign"><span class="sign-name">Lobby · kitchen</span></div>
    <div class="clocks" role="group" aria-label="World clocks">${CLOCKS.map((c, i) => `<div class="clock-tile" data-clock="${i}"><span class="clk-label">${esc(c.label)}</span><span class="clk-time">--:--</span></div>`).join("")}</div>
    <span class="door left" aria-hidden="true"></span><span class="door right" aria-hidden="true"></span>
    <div class="kitchen" aria-hidden="true"><span class="fridge"></span><span class="counter"><span class="sink"></span><span class="espresso"></span><span class="fruit"></span></span><span class="cooler"></span></div>
    ${commonsAgents.length ? `<div class="desks">${commonsAgents.map((a) => workstation(a, "group")).join("")}</div>` : ""}
    <div class="seating" aria-hidden="true">
      <span class="cafe-table"><i></i><i></i><i></i></span>
      <span class="sofa"></span><span class="coffee-table"></span>
      <span class="cafe-table"><i></i><i></i><i></i></span>
    </div>
    <span class="plant corner" aria-hidden="true"></span><span class="plant corner r" aria-hidden="true"></span>
    <span class="reception" aria-hidden="true"></span>
    <span class="entrance" id="entrance" aria-hidden="true"><span class="mat">welcome</span></span>
  </section>`;
}

function build() {
  const group = snap.units.find((u) => u.id === "group") ?? { id: "group", name: "Commons", description: "" };
  const rooms = snap.units.filter((u) => u.id !== "group");
  const roomIds = new Set(rooms.map((u) => u.id));
  const left = rooms.filter((_, i) => i % 2 === 0);
  const right = rooms.filter((_, i) => i % 2 === 1);
  const rows = Math.max(1, left.length, right.length);
  const html = [];
  for (const [side, list] of [["left", left], ["right", right]]) {
    // The last office on a side runs to the bottom when that side has fewer offices.
    list.forEach((u, i) => html.push(officeHTML(u, snap.agents.filter((a) => a.unit === u.id), side, i + 1, i === list.length - 1 ? rows - i : 1, rows)));
  }
  html.push(`<div class="hall left" aria-hidden="true"><span class="runner"></span><span class="exit"></span></div>`);
  html.push(`<div class="hall right" aria-hidden="true"><span class="runner"></span><span class="exit"></span></div>`);
  // Group-level agents (and any whose unit has no office) work in the lobby.
  html.push(`<div class="core-stack">${serverRoomHTML(group)}${lobbyHTML(snap.agents.filter((a) => !roomIds.has(a.unit)))}</div>`);
  floor.style.setProperty("--rows", rows);
  // The core's sync robot, on the floor above every room (only when the core runs it).
  if (snap.robot) html.push(`<button class="robot docked" id="robot" aria-pressed="false">${ROBOT_SVG}<span class="robot-note" role="status"></span></button>`);
  floor.innerHTML = html.join("");
  robotSeq = -1;
  prevMode.clear();
}

// ---------- updates ----------

function feed(html) {
  const ol = $("#feed");
  const li = document.createElement("li");
  li.innerHTML = `<time>${clockTime()}</time><span>${html}</span>`;
  ol.append(li);
  while (ol.children.length > 6) ol.firstElementChild.remove();
  [...ol.children].forEach((el, i, all) => el.classList.toggle("old", i < all.length - 3));
}

function centerOf(el) {
  const r = el.getBoundingClientRect();
  return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
}

const visible = (el) => !!el && el.getClientRects().length > 0 && getComputedStyle(el).display !== "none";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---------- characters ----------
// Each agent is a small top-down person inside its workstation. Body position is flavor; the status
// label (and the ring of its name tag) is the signal: it flips the moment the poll says working,
// even while the person is still walking back from the kitchen.

function spriteOf(a) {
  const h = hash(a.id);
  const pick = (xs, shift) => xs[(h >>> shift) % xs.length];
  const s = a.sprite ?? {};
  return {
    skin: s.skin ?? pick(["#f1c7a5", "#d9a47e", "#c08a62", "#8d5a3b", "#5c3a24"], 1),
    hair: s.hair ?? pick(["#1f1a17", "#3a2a20", "#6b4423", "#a0522d", "#d6b370", "#2b2b2b"], 4),
    hair_style: s.hair_style ?? pick(["short", "long", "updo", "curly", "fade"], 7),
    shirt: s.shirt ?? hueOf(a.unit),
    pants: s.pants ?? "#2a2f38",
    accessory: s.accessory ?? "none",
  };
}

const HAIR_FRONT = {
  short: '<path class="hair" d="M11 13.5Q10.5 4 20 4Q29.5 4 29 13.5Q26.5 8.5 20 8.8Q13.5 8.5 11 13.5Z"/>',
  fade: '<path class="hair" d="M11.6 12Q12 5 20 5Q28 5 28.4 12Q25.5 8.6 20 9Q14.5 8.6 11.6 12Z"/>',
  long: '<path class="hair" d="M10.6 14Q10 4 20 4Q30 4 29.4 14L30 25Q27.4 26 27 22.5L27 12Q20 9 13 12L13 22.5Q12.6 26 10 25Z"/>',
  updo: '<circle class="hair" cx="20" cy="3.6" r="3.6"/><path class="hair" d="M11 13.5Q10.5 4.5 20 4.5Q29.5 4.5 29 13.5Q26.5 8.5 20 8.8Q13.5 8.5 11 13.5Z"/>',
  curly: '<g class="hair"><circle cx="13" cy="9" r="3.6"/><circle cx="17" cy="6" r="3.8"/><circle cx="23" cy="6" r="3.8"/><circle cx="27" cy="9" r="3.6"/><circle cx="11.5" cy="13" r="2.6"/><circle cx="28.5" cy="13" r="2.6"/></g>',
  bald: "",
};
const HAIR_BACK = {
  short: '<circle class="hair" cx="20" cy="13" r="9"/><ellipse cx="20" cy="21.3" rx="4" ry="1.8" class="skin"/>',
  fade: '<circle class="hair" cx="20" cy="12.4" r="8.6"/>',
  long: '<circle class="hair" cx="20" cy="13" r="9.4"/><path class="hair" d="M11 14L10.5 29Q20 32 29.5 29L29 14Z"/>',
  updo: '<circle class="hair" cx="20" cy="13" r="9"/><circle class="hair" cx="20" cy="6" r="4"/>',
  curly: '<g class="hair"><circle cx="20" cy="13" r="9"/><circle cx="12" cy="10" r="3.4"/><circle cx="28" cy="10" r="3.4"/><circle cx="14" cy="18" r="3"/><circle cx="26" cy="18" r="3"/><circle cx="20" cy="5" r="3.6"/></g>',
  bald: "",
};
const HAIR_SIDE = {
  short: '<path class="hair" d="M11 15Q10 4 20 4.5Q27 5 28.6 10.5Q22 8.5 19 10.5Q17.5 14 18.4 18.4Q13 19 11 15Z"/>',
  fade: '<path class="hair" d="M11.6 13Q11.5 5 20 5.2Q26.5 5.5 28 10Q22 8.8 19.4 10.4Q18 13.5 18.6 16.5Q13.6 17 11.6 13Z"/>',
  long: '<path class="hair" d="M11 15Q10 4 20 4.5Q27 5 28.6 10.5Q22 8.5 19.5 10.5Q18.5 16 19 27Q12 27 10.6 24Z"/>',
  updo: '<circle class="hair" cx="12" cy="8" r="3.6"/><path class="hair" d="M11 15Q10 4 20 4.5Q27 5 28.6 10.5Q22 8.5 19 10.5Q17.5 14 18.4 18.4Q13 19 11 15Z"/>',
  curly: '<g class="hair"><circle cx="13" cy="10" r="4"/><circle cx="18" cy="6" r="4"/><circle cx="24" cy="6.5" r="3.4"/><circle cx="12.5" cy="16" r="3.2"/></g>',
  bald: "",
};
const ACC_FRONT = {
  headset: '<path class="acc" d="M10.8 13.5Q10.8 3.4 20 3.4Q29.2 3.4 29.2 13.5"/><rect class="acc-f" x="8.8" y="11.6" width="3.2" height="5.4" rx="1.2"/><path class="acc" d="M10.6 16.6Q12 20 16.4 19.6"/>',
  glasses: '<g class="acc"><circle cx="16.8" cy="15" r="2.3"/><circle cx="23.2" cy="15" r="2.3"/><path d="M19.1 15H20.9"/></g>',
  cap: '<path class="acc-f" d="M10.8 12Q11 3.6 20 3.6Q29 3.6 29.2 12Z"/><rect class="acc-f" x="14" y="10.6" width="12" height="2.6" rx="1.2"/>',
  earrings: '<circle class="gold" cx="11" cy="17.5" r="1.1"/><circle class="gold" cx="29" cy="17.5" r="1.1"/>',
};
const ACC_BACK = {
  headset: '<path class="acc" d="M11 12.4Q20 5 29 12.4"/><rect class="acc-f" x="8.8" y="11.6" width="3.2" height="5.4" rx="1.2"/>',
  cap: '<circle class="acc-f" cx="20" cy="12" r="8.8"/>',
  earrings: '<circle class="gold" cx="11" cy="17.5" r="1.1"/><circle class="gold" cx="29" cy="17.5" r="1.1"/>',
};
const ACC_SIDE = {
  headset: '<path class="acc" d="M13 8Q20 2.6 25 6"/><circle class="acc-f" cx="18.6" cy="14.6" r="2.4"/><path class="acc" d="M18.6 16.6Q20 20 25.4 19.4"/>',
  glasses: '<g class="acc"><circle cx="25" cy="14.5" r="2.2"/><path d="M22.8 14.2H19"/></g>',
  cap: '<path class="acc-f" d="M11 12Q11 4 20 4Q28 4.4 28.6 10Z"/><rect class="acc-f" x="24" y="9.4" width="8" height="2.4" rx="1.2"/>',
  earrings: '<circle class="gold" cx="18.6" cy="18" r="1.1"/>',
};

// Arms hang from the shoulder; poses rotate them (transform-box: view-box) in style.css.
const ARMS = `<g class="arm l"><rect x="7.6" y="25" width="5" height="14" rx="2.5" class="shirt"/><circle cx="10.1" cy="39.4" r="2.4" class="skin"/></g>
  <g class="arm r"><rect x="27.4" y="25" width="5" height="14" rx="2.5" class="shirt"/><circle cx="29.9" cy="39.4" r="2.4" class="skin"/></g>`;
const LEGS = `<g class="legs"><g class="leg l"><rect x="14.4" y="40" width="5" height="13" rx="2" class="pants"/><rect x="13.9" y="51" width="6" height="4" rx="2" class="shoe"/></g>
  <g class="leg r"><rect x="20.6" y="40" width="5" height="13" rx="2" class="pants"/><rect x="20.1" y="51" width="6" height="4" rx="2" class="shoe"/></g></g>
  <g class="legs-seated"><rect x="14.4" y="40" width="5" height="7" rx="2" class="pants"/><rect x="20.6" y="40" width="5" height="7" rx="2" class="pants"/><rect x="13.9" y="45" width="6" height="4" rx="2" class="shoe"/><rect x="20.1" y="45" width="6" height="4" rx="2" class="shoe"/></g>`;
const CUP = '<g class="cup"><rect x="27.6" y="36" width="5" height="5.6" rx="1" fill="#f8fafc"/><rect x="28.2" y="36.6" width="3.8" height="1.6" fill="#6b4226"/></g>';

function charSVG(a) {
  const s = spriteOf(a);
  const style = `--skin:${s.skin};--hair:${s.hair};--shirt:${s.shirt};--pants:${s.pants}`;
  const torso = '<rect x="11" y="24" width="18" height="19" rx="6" class="shirt"/><rect x="17.5" y="21" width="5" height="4" class="skin"/>';
  return `<svg class="char" viewBox="0 0 40 60" style="${style}" aria-hidden="true">
    <g class="v v-front">${LEGS}${torso}<path d="M17 24.4L20 28L23 24.4" class="collar"/>${ARMS}${CUP}
      <circle cx="20" cy="14" r="9" class="skin"/>${HAIR_FRONT[s.hair_style] ?? ""}
      <circle cx="16.8" cy="15" r="1.15" class="eye"/><circle cx="23.2" cy="15" r="1.15" class="eye"/><path d="M18 18.6Q20 19.9 22 18.6" class="mouth"/>
      ${ACC_FRONT[s.accessory] ?? ""}</g>
    <g class="v v-back">${LEGS}${torso}${ARMS}<circle cx="20" cy="14" r="9" class="skin"/>${HAIR_BACK[s.hair_style] ?? ""}${ACC_BACK[s.accessory] ?? ""}</g>
    <g class="v v-side"><g class="legs"><g class="leg l"><rect x="15.5" y="40" width="5.5" height="13" rx="2" class="pants"/><rect x="15.5" y="51" width="8" height="4" rx="2" class="shoe"/></g>
      <g class="leg r"><rect x="17.5" y="40" width="5.5" height="13" rx="2" class="pants"/><rect x="17.5" y="51" width="8" height="4" rx="2" class="shoe"/></g></g>
      <rect x="13" y="24" width="14" height="19" rx="6" class="shirt"/><rect x="17.5" y="21" width="5" height="4" class="skin"/>
      <g class="arm s"><rect x="17.5" y="25" width="5" height="14" rx="2.5" class="shirt"/><circle cx="20" cy="39.4" r="2.4" class="skin"/></g>
      <circle cx="20" cy="14" r="9" class="skin"/><circle cx="28.6" cy="15.6" r="1.4" class="skin"/>${HAIR_SIDE[s.hair_style] ?? ""}
      <circle cx="25" cy="14.6" r="1.15" class="eye"/>${ACC_SIDE[s.accessory] ?? ""}</g>
  </svg>`;
}

const SOURCES = { slack: "Slack", cron: "scheduled job", api_server: "API run", api: "API run", telegram: "Telegram", cli: "terminal", discord: "Discord", whatsapp: "WhatsApp" };
const sourceLabel = (s) => (s ? SOURCES[s] ?? s.replace(/[_-]/g, " ") : "a task");

function idleFor(a) {
  // Since the last conversation or the last scheduled job, whichever came later.
  const t = Math.max(Date.parse(a.activity?.lastActive ?? "") || 0, Date.parse(a.activity?.lastJobAt ?? "") || 0);
  if (!t) return "";
  const m = Math.round((serverNow() - t) / 60000);
  return m < 1 ? "just now" : m < 60 ? `${m}m` : m < 2880 ? `${Math.round(m / 60)}h` : `${Math.round(m / 1440)}d`;
}

/** The status line under an agent's name: what it is doing, not where its body is. */
/** A tool's name as people read it: MCP tools without their `mcp_<server>_` prefix, underscores as spaces. */
const toolLabel = (t) => String(t ?? "").replace(/^mcp_[a-z0-9]+_/, "").replace(/_/g, " ");

/** What the agent is working on: the tool it is running (its hooks), a scheduled job when that is the only thing running, else the newest session's source. */
function workLabel(a) {
  if (a.live?.tool) return toolLabel(a.live.tool);
  const jobs = a.activity?.runningJobs ?? [];
  if (jobs.length && !(a.activity?.activeAgents > 0)) return jobs[0];
  return sourceLabel(a.activity?.source);
}

function statusText(a, mode, p) {
  if (mode === "busy") return `Working · ${workLabel(a)}`;
  if (mode === "idle") return `Idle${idleFor(a) ? ` ${idleFor(a)}` : ""}${a.office?.label ? ` · ${a.office.label}` : ""}`;
  return { arriving: "Arriving", alert: "Needs attention", unknown: "Status unknown", away: "Off shift", leaving: "Leaving" }[mode] ?? mode;
}

// The core runs the office (pkg/core/office.go): each agent's activity, phase and timings come with
// every poll, and this page only plays them back by server time, so every viewer sees the same thing.
// Paths depend on this viewer's layout; the timing does not: walks are paced to end when the phase ends.
const LOBBY = new Set(["coffee", "water", "sofa"]);
const TRIP = new Set(["coffee", "water", "sofa", "window", "stretch"]);
let clockOffset = 0; // server time minus this browser's
const serverNow = () => Date.now() + clockOffset;

const people = new Map(); // agent id → person

function person(id) {
  const ws = floor.querySelector(`.ws[data-agent="${CSS.escape(id)}"]`);
  if (!ws) return null;
  let p = people.get(id);
  if (!p || p.ws !== ws) {
    p = { id, ws, el: $(".agent", ws), pose: "", facing: "", token: 0, seq: -1, path: null, leg: 0, walking: false };
    people.set(id, p);
  }
  return p;
}

function setPose(p, pose, facing) {
  if (pose !== p.pose) {
    p.el.classList.remove(`pose-${p.pose}`);
    p.el.classList.add(`pose-${pose}`);
    p.pose = pose;
  }
  if (facing && facing !== p.facing) {
    p.el.classList.remove(`face-${p.facing}`);
    p.el.classList.add(`face-${facing}`);
    p.facing = facing;
  }
  p.ws.classList.toggle("relaxing", pose === "sit-relax");
  p.ws.classList.toggle("empty-chair", !pose.startsWith("sit"));
  // Someone is out of their chair: lift their office over the hallways and lobby so they stay visible.
  const room = p.ws.closest(".room, .lobby");
  room?.classList.toggle("outing", !!room.querySelector(".ws.empty-chair:not(.away)"));
}

const seatPose = (working) => (working ? "sit-type" : "sit-idle");
const zoomOf = (p) => parseFloat(getComputedStyle(p.ws.parentElement).zoom) || 1;
const seatOf = (p) => centerOf($(".chair", p.ws));
const visiblePlace = (sel) => [...floor.querySelectorAll(sel)].find(visible);

/** Where the person is now, in page pixels (its torso; the chair centre when seated). */
function hereOf(p) {
  const seat = seatOf(p);
  const t = getComputedStyle(p.el).transform;
  const m = new DOMMatrixReadOnly(t === "none" ? undefined : t);
  const z = zoomOf(p);
  return { x: seat.x + m.e * z, y: seat.y + m.f * z };
}

function place(p, pt) {
  const seat = seatOf(p);
  const z = zoomOf(p);
  p.el.style.transform = pt ? `translate(${(pt.x - seat.x) / z}px, ${(pt.y - seat.y) / z}px)` : "";
}

/** A spot in page pixels, with the pose and facing to hold there. `slot` puts people side by side. */
function spotOf(p, name, slot = 0) {
  const room = p.ws.closest(".room, .lobby");
  const seat = seatOf(p);
  const r = room.getBoundingClientRect();
  const side = slot === 0 ? 0 : (slot % 2 ? 1 : -1) * Math.ceil(slot / 2) * 26;
  const at = (sel, dx, dy, pose, face) => {
    const e = visiblePlace(sel);
    return e && { x: centerOf(e).x + dx + side, y: centerOf(e).y + dy, pose, face, where: "lobby" };
  };
  if (name === "coffee") return at(".espresso", 0, 30, "coffee", "down");
  if (name === "water") return at(".cooler", -6, 30, "coffee", "down");
  if (name === "sofa") return at(".sofa", -18 + (slot % 2) * 36 - side, -8, "sofa", "down");
  if (name === "window") {
    const w = [...room.querySelectorAll(".win.left, .win.right")].find(visible);
    if (!w) return null;
    const c = centerOf(w);
    const left = w.classList.contains("left");
    return { x: c.x + (left ? 34 : -34), y: Math.min(Math.max(seat.y, r.top + 70), r.bottom - 40) + side, pose: "stand", face: left ? "left" : "right", where: "room" };
  }
  if (name === "stretch") return { x: seat.x + 34, y: seat.y + 18, pose: "stretch", face: "up", where: "room" };
  if (name === "entrance") {
    const e = $("#entrance");
    return visible(e) && { ...centerOf(e), y: centerOf(e).y - 6, where: "lobby" };
  }
  return null;
}

/**
 * Seat to spot, in page pixels: up from the chair into the aisle, then for the lobby out through the
 * office door, along the hallway, in through the lobby's side door. null when there is no way (no
 * hallways on narrow screens).
 */
function pathTo(p, spot) {
  const room = p.ws.closest(".room, .lobby");
  const seat = seatOf(p);
  const aisle = { x: seat.x, y: seat.y + 40 };
  if (spot.where === "room" || room.classList.contains("lobby")) return [seat, aisle, { x: spot.x, y: aisle.y }, spot];
  const side = room.classList.contains("right") ? "right" : "left";
  const hall = $(`.hall.${side}`);
  const lobbyDoor = $(`.lobby .door.${side}`);
  const door = $(".door", room);
  if (!visible(hall) || !lobbyDoor || !door) return null;
  const d = centerOf(door), h = centerOf(hall), ld = centerOf(lobbyDoor);
  const inward = side === "left" ? -28 : 28; // into the office from its door
  return [seat, aisle, { x: d.x + inward, y: aisle.y }, { x: d.x + inward, y: d.y }, d, { x: h.x, y: d.y }, { x: h.x, y: ld.y }, ld, { x: ld.x - inward, y: ld.y }, { x: spot.x, y: ld.y }, spot];
}

const length = (pts) => pts.slice(1).reduce((n, b, i) => n + Math.hypot(b.x - pts[i].x, b.y - pts[i].y), 0);

/** The rest of a path from a fraction of its length on: [point at f, …later points]. */
function from(pts, f) {
  let d = Math.max(0, Math.min(1, f)) * length(pts);
  for (let i = 1; i < pts.length; i++) {
    const a = pts[i - 1], b = pts[i], seg = Math.hypot(b.x - a.x, b.y - a.y);
    if (d <= seg) {
      const t = seg ? d / seg : 1;
      return [{ x: a.x + (b.x - a.x) * t, y: a.y + (b.y - a.y) * t }, ...pts.slice(i)];
    }
    d -= seg;
  }
  return [pts[pts.length - 1]];
}

/** Walk a path segment by segment, facing the way it goes. false when interrupted. */
async function walkPath(p, pts, token, speed) {
  p.path = pts;
  p.walking = true;
  setPose(p, "walk");
  for (let i = 1; i < pts.length; i++) {
    if (token !== p.token) return false;
    const a = pts[i - 1], b = pts[i];
    const dx = b.x - a.x, dy = b.y - a.y, len = Math.hypot(dx, dy);
    p.leg = i;
    if (len < 1) continue;
    setPose(p, "walk", Math.abs(dx) > Math.abs(dy) ? (dx > 0 ? "right" : "left") : dy > 0 ? "down" : "up");
    const seat = seatOf(p), z = zoomOf(p);
    const to = `translate(${(b.x - seat.x) / z}px, ${(b.y - seat.y) / z}px)`;
    const duration = (len / speed) * 1000;
    const anim = p.el.animate([{ transform: getComputedStyle(p.el).transform }, { transform: to }], { duration, easing: "linear", fill: "forwards" });
    p.anim = anim;
    // A hidden tab renders no frames and never settles `finished`; the timer keeps the walk going.
    const cancelled = await Promise.race([anim.finished.then(() => false, () => true), sleep(duration + 60).then(() => false)]);
    if (cancelled || token !== p.token) return false;
    p.el.style.transform = to;
    anim.cancel();
  }
  p.walking = false;
  return true;
}

function stopWalking(p) {
  if (p.anim) {
    try {
      p.anim.commitStyles();
    } catch {}
    p.anim.cancel();
    p.anim = null;
  }
}

/** Play the rest of `pts` from fraction `f`, arriving when the phase ends; then `done`. */
async function play(p, pts, f, endsAt, done) {
  const token = ++p.token;
  stopWalking(p);
  const rest = from(pts, f);
  const ms = endsAt - serverNow();
  if (reducedMotion.matches || rest.length < 2 || ms < 250) {
    place(p, rest[rest.length - 1]);
    p.walking = false;
    return token === p.token && done();
  }
  place(p, rest[0]);
  if (await walkPath(p, rest, token, Math.max(30, length(rest) / (ms / 1000)))) done();
}

/** Where to start a walk back: from where this person is if it is out walking, else the spot. */
function backPath(p, outbound) {
  if (p.walking && p.path) return [hereOf(p), ...p.path.slice(0, p.leg).reverse()];
  return [...outbound].reverse();
}

/**
 * Bring this person to the core's state for it. A new `seq` starts the phase, from where it is in
 * time (a viewer who loads mid-walk joins mid-walk); otherwise only the seated pose follows work.
 */
function follow(p, a, mode) {
  const o = a.office;
  const present = PRESENT.has(mode);
  if (!o) {
    // A core without the office simulation: be at the desk or gone, nothing else.
    p.ws.classList.toggle("away", !present);
    if (p.seq !== -2) {
      p.seq = -2;
      p.token++;
      stopWalking(p);
      place(p, null);
    }
    return setPose(p, seatPose(mode === "busy"), "up");
  }
  if (o.seq === p.seq) {
    if (o.activity === "desk") setPose(p, seatPose(o.working), "up");
    return;
  }
  const first = p.seq < 0;
  p.seq = o.seq;
  const f = o.end ? (serverNow() - o.start) / (o.end - o.start) : 1;
  const seated = () => {
    p.token++;
    stopWalking(p);
    p.walking = false;
    p.path = null;
    place(p, null);
    setPose(p, o.activity === "relax" ? "sit-relax" : seatPose(o.working), "up");
  };
  p.ws.classList.toggle("away", o.activity === "away");

  if (o.activity === "away") return seated();
  if (o.activity === "desk" || o.activity === "relax") {
    // Walking back already (this viewer's walk ends on the same clock): let it finish.
    if (p.walking && !first) return;
    return seated();
  }

  const entrance = spotOf(p, "entrance");
  const toEntrance = entrance && pathTo(p, { ...entrance, where: "lobby" });
  if (o.activity === "arrive") {
    if (!toEntrance) return seated();
    return play(p, [...toEntrance].reverse(), f, o.end, seated);
  }
  if (o.activity === "leave") {
    const spot = TRIP.has(o.from) && spotOf(p, o.from, o.slot);
    const out = p.walking && p.path ? [hereOf(p), ...(spot && LOBBY.has(o.from) ? [] : p.path.slice(0, p.leg).reverse())] : spot && LOBBY.has(o.from) ? [spot] : [seatOf(p)];
    const tail = LOBBY.has(o.from) && entrance ? [entrance] : toEntrance ?? [];
    const path = [...out, ...tail.slice(LOBBY.has(o.from) ? 0 : 1)];
    if (!entrance || path.length < 2) {
      p.ws.classList.add("away");
      return seated();
    }
    return play(p, path, first ? f : 0, o.end, () => {
      p.ws.classList.add("away");
      seated();
    });
  }

  // Trips: going, there, returning.
  const spot = spotOf(p, o.activity, o.slot);
  const path = spot && pathTo(p, spot);
  if (!path) {
    // No way there on this layout (no hallways): stay seated; the name tag still says where.
    return seated();
  }
  const arrived = () => {
    p.walking = false;
    place(p, spot);
    setPose(p, spot.pose, spot.face);
  };
  if (o.phase === "going") return play(p, path, f, o.end, arrived);
  if (o.phase === "there") {
    p.token++;
    stopWalking(p);
    return arrived();
  }
  // returning: from where this viewer's person is, or the spot for a viewer who just arrived
  const back = first ? [...path].reverse() : backPath(p, path);
  return play(p, back, first ? f : 0, o.end, seated);
}

/** Everyone re-joins the core's timeline (after a resize, or when the tab becomes visible). */
// ---------- the sync robot ----------
// The core's robot (pkg/core/office.go) rolls from its dock in the server room to an agent's desk when
// that agent's nap lands in the store, writes down what changed, and goes on or back. Like the people,
// it is played back by server time; its route is drawn for this viewer's layout.

const ROBOT_SVG = `<svg viewBox="0 0 30 34" aria-hidden="true">
  <ellipse cx="15" cy="32" rx="9" ry="2.4" fill="#0008"/>
  <rect x="8" y="26" width="14" height="5" rx="2.5" fill="#334155"/>
  <circle class="wheel" cx="11" cy="29.6" r="2" fill="#0f172a" stroke="#64748b" stroke-width="0.8"/>
  <circle class="wheel" cx="19" cy="29.6" r="2" fill="#0f172a" stroke="#64748b" stroke-width="0.8"/>
  <rect x="4.6" y="16" width="2.6" height="7" rx="1.3" fill="#cbd5e1"/>
  <rect x="6" y="14" width="18" height="13" rx="4" fill="#e2e8f0"/>
  <rect x="10" y="17" width="10" height="5" rx="1" fill="#0f172a"/>
  <circle class="led" cx="12.4" cy="19.5" r="0.9" fill="#34d399"/><circle class="led b" cx="15" cy="19.5" r="0.9" fill="#67e8f9"/><circle class="led c" cx="17.6" cy="19.5" r="0.9" fill="#fbbf24"/>
  <rect x="7" y="3" width="16" height="11" rx="4" fill="#f8fafc"/>
  <rect x="9" y="5" width="12" height="7" rx="2" fill="#0f172a"/>
  <g class="eyes"><rect x="11.2" y="7.4" width="2.4" height="2.4" rx="1.2" fill="#67e8f9"/><rect x="16.4" y="7.4" width="2.4" height="2.4" rx="1.2" fill="#67e8f9"/></g>
  <line x1="15" y1="3" x2="15" y2="0.9" stroke="#94a3b8" stroke-width="1"/><circle class="tip" cx="15" cy="0.9" r="1.3" fill="#34d399"/>
  <g class="clip"><rect x="21.4" y="15.4" width="7" height="9.4" rx="1" fill="#fef3c7" stroke="#92400e" stroke-width="0.8"/>
    <path d="M23 18.4h4M23 20.4h4M23 22.4h2.6" stroke="#a16207" stroke-width="0.6"/>
    <path class="pen" d="M26.6 23.6l2.4-3.4" stroke="#1e3a8a" stroke-width="1.2" stroke-linecap="round"/></g>
</svg>`;

let robotSeq = -1;
let robotToken = 0;
let robotAnim = null;
let lastNoteLogged = 0;

const agentName = (id) => snap?.agents.find((a) => a.id === id)?.name ?? id;

/** "Atlas's nap · 2 learning, 1 state" (counts per class; no file names ever reach the page). */
function noteText(n) {
  if (!n) return "";
  const c = n.changed ?? {};
  const parts = [
    c.learning && `${c.learning} learning`,
    c.state && `${c.state} state`,
    c.raw && `${c.raw} conversation log${c.raw === 1 ? "" : "s"}`,
    n.removed && `${n.removed} removed`,
  ].filter(Boolean);
  const what = n.reason === "shutdown" ? "shutdown nap" : n.naps > 1 ? `${n.naps} naps` : "nap";
  return `${agentName(n.agent)}'s ${what} · ${parts.join(", ") || "no changes"}`;
}

function floorPoint(pt) {
  const f = floor.getBoundingClientRect();
  return { x: pt.x - f.left, y: pt.y - f.top };
}

/** Points in page pixels from a place to the lobby hub: the dock, or the spot beside an agent's chair. */
function toHub(node) {
  const sdoor = $(".server .door.bottom");
  const lobby = $(".lobby");
  if (!sdoor || !visible(lobby)) return null;
  const sd = centerOf(sdoor);
  const hub = { x: sd.x, y: lobby.getBoundingClientRect().top + 96 };
  if (node === "dock") {
    const rack = $("#rack");
    if (!visible(rack)) return null;
    const r = rack.getBoundingClientRect();
    return [{ x: r.left + r.width / 2, y: r.bottom + 26 }, { x: sd.x, y: sd.y - 26 }, sd, { x: sd.x, y: sd.y + 26 }, hub];
  }
  const ws = floor.querySelector(`.ws[data-agent="${CSS.escape(node)}"]`);
  if (!ws) return null;
  const seat = centerOf($(".chair", ws));
  const spot = { x: seat.x + 46, y: seat.y + 4 };
  const room = ws.closest(".room, .lobby");
  if (room.classList.contains("lobby")) return [spot, hub];
  const side = room.classList.contains("right") ? "right" : "left";
  const hall = $(`.hall.${side}`), ld = $(`.lobby .door.${side}`), door = $(".door", room);
  if (!visible(hall) || !ld || !door) return null;
  const d = centerOf(door), h = centerOf(hall), l = centerOf(ld);
  const inward = side === "left" ? -28 : 28;
  const aisle = seat.y + 40;
  return [spot, { x: spot.x, y: aisle }, { x: d.x + inward, y: aisle }, { x: d.x + inward, y: d.y }, d, { x: h.x, y: d.y }, { x: h.x, y: l.y }, l, { x: l.x - inward, y: l.y }, { x: l.x - inward, y: hub.y }, hub];
}

/** From one place to another through the lobby hub, in floor coordinates. */
function robotRoute(from, to) {
  const a = toHub(from), b = toHub(to);
  if (!a || !b) return null;
  return [...a, ...[...b].reverse().slice(1)].map(floorPoint);
}

function robotAt(el, pt, facing) {
  el.style.transform = `translate(${pt.x}px, ${pt.y}px)`;
  if (facing) el.classList.toggle("face-left", facing === "left");
}

async function robotMove(el, pts, f, endsAt, token) {
  robotAnim?.cancel();
  const rest = from(pts, f);
  const ms = endsAt - serverNow();
  if (reducedMotion.matches || rest.length < 2 || ms < 250) return robotAt(el, rest[rest.length - 1]);
  const speed = length(rest) / ms; // px per ms
  robotAt(el, rest[0]);
  el.classList.add("rolling");
  for (let i = 1; i < rest.length; i++) {
    if (token !== robotToken) return;
    const a = rest[i - 1], b = rest[i];
    const len = Math.hypot(b.x - a.x, b.y - a.y);
    if (len < 1) continue;
    if (Math.abs(b.x - a.x) > 2) el.classList.toggle("face-left", b.x < a.x);
    const to = `translate(${b.x}px, ${b.y}px)`;
    const duration = len / speed;
    robotAnim = el.animate([{ transform: getComputedStyle(el).transform }, { transform: to }], { duration, easing: "linear", fill: "forwards" });
    const cancelled = await Promise.race([robotAnim.finished.then(() => false, () => true), sleep(duration + 60).then(() => false)]);
    if (cancelled || token !== robotToken) return;
    el.style.transform = to;
    robotAnim.cancel();
  }
  el.classList.remove("rolling");
}

function followRobot(r) {
  const el = $("#robot");
  if (!el || !r) return;
  const bubble = $(".robot-note", el);
  bubble.textContent = r.phase === "there" ? noteText(r.note) : "";
  el.classList.toggle("writing", r.phase === "there");
  el.classList.toggle("docked", r.phase === "docked");
  el.setAttribute("aria-label", r.phase === "there" ? `Sync robot: ${noteText(r.note)}` : `Sync robot, ${r.phase}`);
  el.setAttribute("aria-pressed", String(selected?.kind === "robot"));
  if (r.seq === robotSeq) return;
  const first = robotSeq < 0;
  robotSeq = r.seq;
  const token = ++robotToken;
  robotAnim?.cancel();
  el.classList.remove("rolling");
  if (r.phase === "there" && r.note && r.note.at !== lastNoteLogged) {
    lastNoteLogged = r.note.at;
    if (!first) feed(`Sync robot noted <b>${esc(noteText(r.note))}</b>`);
  }
  const place = (node) => {
    // No way to that desk on this layout (no hallways): the robot waits at its dock, the note still shows.
    const pts = toHub(node) ?? toHub("dock");
    if (pts) robotAt(el, floorPoint(pts[0]));
    el.hidden = !pts;
  };
  if (r.phase === "docked") return place("dock");
  if (r.phase === "there") return place(r.target);
  const route = robotRoute(r.from, r.target ?? "dock");
  if (!route) return place(r.phase === "returning" ? "dock" : r.target);
  el.hidden = false;
  const f = r.end ? (serverNow() - r.start) / (r.end - r.start) : 1;
  robotMove(el, route, f, r.end, token);
}

function robotPanel() {
  const r = snap.robot;
  const where = r.phase === "docked" ? "At its dock in the server room" : r.phase === "there" ? `Writing at ${esc(agentName(r.target))}'s desk` : r.phase === "returning" ? "Heading back to the dock" : `On the way to ${esc(agentName(r.target))}`;
  return `<div style="--ring:#67e8f9;--hue:#67e8f9">
    <div class="who"><div class="portrait robot-portrait" aria-hidden="true">${ROBOT_SVG}</div>
      <div><h2>Sync robot</h2><div class="chips"><span class="chip" style="--c:#67e8f9"><span class="pip"></span>${esc(r.phase)}</span></div></div></div>
    <p class="role">Each agent's nap sidecar saves its memory, skills and state to the store on its own schedule. When a new nap lands, the core's robot goes to that desk and writes down what changed. It shows the sync; it does not cause it.</p>
    <div class="bars">
      <div class="tile"><b>${r.queue.length}</b><span>queued</span></div>
      <div class="tile"><b>${r.log.length}</b><span>notes</span></div>
      <div class="tile"><b>${r.log[0] ? ago(new Date(r.log[0].at).toISOString()) : "—"}</b><span>last note</span></div>
    </div>
    <h3>Now</h3><p class="role">${where}${r.queue.length ? ` · next: ${r.queue.map((q) => esc(agentName(q))).join(", ")}` : ""}</p>
    <h3>Notebook</h3>
    ${r.log.length ? `<ol class="notebook">${r.log.map((n) => `<li><time>${clockTime(new Date(n.at))}</time>${esc(noteText(n))}</li>`).join("")}</ol>` : '<p class="note">No naps since the core started. The first nap after a restart is not visited: there is nothing to compare it with.</p>'}
    <p class="note">Counts per kind of file: learning (memory, skills), state (databases), conversation logs. File names and contents never leave the store.</p>
    <h3>Commands</h3>
    <div class="cmds">${cmdButton("swarm learn", "nap now + dream")}</div>
  </div>`;
}

function resync() {
  robotSeq = -1;
  for (const p of people.values()) {
    p.token++;
    stopWalking(p);
    p.walking = false;
    p.path = null;
    p.seq = -1;
  }
  if (snap) refresh();
}

function updateAgent(a, gw) {
  const p = person(a.id);
  if (!p) return;
  const ws = p.ws;
  const mode = modeOf(a, gw);
  const before = prevMode.get(a.id);
  prevMode.set(a.id, mode);
  ws.dataset.mode = mode;
  ws.classList.toggle("selected", selected?.kind === "agent" && selected.id === a.id);
  ws.classList.toggle("thinking", (gw?.active?.[a.id] ?? 0) > 0);

  if (before !== undefined && before !== mode) logTransition(a, before, mode);
  const trip = a.office?.activity;
  if (a.office && p.seq >= 0 && a.office.seq !== p.seq && LOBBY.has(trip) && a.office.phase === "going" && Date.now() - (p.lastTripLog ?? 0) > 180000) {
    p.lastTripLog = Date.now();
    feed(`<b>${esc(a.name)}</b> is ${esc(a.office.label)}`);
  }
  if (a.office && p.seq >= 0 && a.office.seq !== p.seq && a.office.phase === "returning" && a.office.working) {
    feed(`<b>${esc(a.name)}</b> heads back to the desk · ${esc(workLabel(a))}`);
  }
  follow(p, a, mode);

  const r = review(a);
  const marker = $(".marker.review", ws);
  marker.textContent = r ? String(r) : "";
  marker.classList.toggle("on", r > 0);
  marker.title = `${a.pendingLearnings} learnings, ${a.pendingSkills} skill proposals waiting for review`;
  $(".marker.alert", ws).classList.toggle("on", mode === "alert");
  $(".tag .what", ws).textContent = statusText(a, mode, p);
  $(".bubble .src", ws).textContent = workLabel(a);
  const label = `${a.name}, ${statusText(a, mode, p)}${r ? `, ${r} to review` : ""}`;
  p.el.setAttribute("aria-label", label);
  p.el.setAttribute("aria-pressed", String(selected?.kind === "agent" && selected.id === a.id));

  const pr = prevReview.get(a.id);
  if (pr !== undefined && r > pr) feed(`<b>${esc(a.name)}</b> has ${r} item${r === 1 ? "" : "s"} waiting for review`);
  prevReview.set(a.id, r);
}

function logTransition(a, from, to) {
  const n = `<b>${esc(a.name)}</b>`;
  if (to === "busy") {
    if (Date.now() - (lastBusyLog.get(a.id) ?? 0) < 60_000) return;
    lastBusyLog.set(a.id, Date.now());
    return feed(`${n} started working · ${esc(workLabel(a))}`);
  }
  if (from === "busy" && to === "idle") return;
  const msg = {
    arriving: `${n} is on the way in`,
    idle: `${n} is on shift`,
    alert: `${n} needs attention: ${esc(a.detail ?? a.health ?? "a platform needs attention")}`,
    unknown: `Can't see ${n}'s desk${a.detail ? ` (${esc(a.detail)})` : ""}`,
    away: `${n} left the office`,
  }[to];
  if (msg) feed(msg);
}

function updateGateway(gw) {
  const rack = $("#rack");
  if (!rack) return;
  const limited = !!gw.planLimitedUntil;
  const color = limited ? "var(--bad)" : gw.login === "ok" ? "var(--ok)" : "var(--warn)";
  rack.style.setProperty("--rack", color);
  $("#rack-sub").textContent = limited ? `plan limit · ${clockTime(new Date(gw.planLimitedUntil))}` : gw.login === "ok" ? "Using ChatGPT plan" : "not signed in";
  $("#slots").innerHTML = Array.from({ length: gw.concurrency }, (_, i) => `<span class="slot${i < gw.inflight ? " on" : ""}"></span>`).join("");
  rack.setAttribute("aria-pressed", String(selected?.kind === "core"));
  // One blade per agent container in the fleet rack, lit by its state.
  $("#blades").innerHTML = snap.agents
    .map((a) => `<span class="blade" style="--c:${MODE[modeOf(a, gw)].color}" title="${esc(a.name)}: ${esc(MODE[modeOf(a, gw)].label)}"><i></i>${esc(a.id)}</span>`)
    .join("");
  if (prevGateway) {
    if (prevGateway.login !== gw.login) feed(gw.login === "ok" ? "<b>Core</b> signed in to the ChatGPT plan" : `<b>Core</b> login: ${esc(gw.login)}`);
    if (!prevGateway.planLimitedUntil && limited) feed(`<b>Core</b> hit the plan limit; agents use their fallback until ${clockTime(new Date(gw.planLimitedUntil))}`);
    if (prevGateway.planLimitedUntil && !limited) feed("<b>Core</b> plan limit lifted");
  }
  prevGateway = gw;
}

function meters() {
  const gw = snap.gateway;
  const modes = snap.agents.map((a) => modeOf(a, gw));
  const onShift = modes.filter((m) => m !== "away" && m !== "unknown").length;
  const working = modes.filter((m) => m === "busy").length;
  const idle = modes.filter((m) => m === "idle").length;
  const alerts = modes.filter((m) => m === "alert").length;
  const toReview = snap.agents.reduce((n, a) => n + review(a), 0);
  const usage = Object.values(gw.usage ?? {});
  const calls = usage.reduce((n, u) => n + u.requests, 0);
  const tokens = usage.reduce((n, u) => n + u.inputTokens + u.outputTokens, 0);
  const plan = gw.planLimitedUntil ? ["limited", "var(--bad)"] : gw.login === "ok" ? ["online", "var(--ok)"] : [gw.login === "missing" ? "signed out" : gw.login, "var(--warn)"];
  const items = [
    ["On shift", `${onShift}/${snap.agents.length}`, onShift ? "var(--ok)" : "var(--off)"],
    ["Working", working, working ? "var(--busy)" : null],
    ["Idle", idle, idle ? "var(--ok)" : null],
    ["To review", toReview, toReview ? "var(--gold)" : null],
    ...(alerts ? [["Attention", alerts, "var(--bad)"]] : []),
    [gw.login === "ok" && !gw.planLimitedUntil ? "Using ChatGPT plan" : "ChatGPT plan", gw.login === "ok" && !gw.planLimitedUntil ? "" : plan[0], plan[1]],
    ["LLM", `${compact(calls)} calls · ${compact(tokens)} tok`, null],
  ];
  $("#meters").innerHTML = items
    .map(([k, v, c]) => `<li class="meter${c && c !== "var(--ok)" && c !== "var(--off)" ? " alert" : ""}" style="${c ? `--c:${c}` : ""}"><span class="pip"></span>${k} <b>${esc(v)}</b></li>`)
    .join("") +
    (gw.planLimitedUntil && gw.manageUsageUrl
      ? `<li><a class="meter alert manage" style="--c:var(--bad)" href="${esc(gw.manageUsageUrl)}" target="_blank" rel="noopener noreferrer"><span class="pip"></span><b>Manage usage ↗</b></a></li>`
      : "");
}

function drawLinks() {
  const rack = $("#rack");
  const box = links.getBoundingClientRect();
  links.setAttribute("viewBox", `0 0 ${box.width} ${box.height}`);
  if (!rack || !snap) return (links.innerHTML = "");
  const r = centerOf(rack);
  const paths = [];
  for (const a of snap.agents) {
    if ((snap.gateway.active?.[a.id] ?? 0) === 0) continue; // model call in flight through the core
    const face = floor.querySelector(`.ws[data-agent="${CSS.escape(a.id)}"] .screen`);
    if (!face) continue;
    const f = centerOf(face);
    const x1 = r.x - box.left, y1 = r.y - box.top, x2 = f.x - box.left, y2 = f.y - box.top;
    const mx = (x1 + x2) / 2, my = Math.min(y1, y2) - 60;
    paths.push(`<path d="M${x1},${y1} Q${mx},${my} ${x2},${y2}" stroke="${hueOf(a.unit)}"/>`);
  }
  links.innerHTML = paths.join("");
}

// ---------- inspector ----------

function cmdButton(cmd, hint) {
  return `<button class="cmd" data-copy="${esc(cmd)}"><span>${esc(cmd)}</span><span class="hint">${esc(hint)}</span></button>`;
}

function agentPanel(a) {
  const gw = snap.gateway;
  const mode = modeOf(a, gw);
  const u = gw.usage?.[a.id];
  const stale = a.lastNap && PRESENT.has(mode) && Date.now() - Date.parse(a.lastNap) > 2 * a.napIntervalSeconds * 1000;
  const cmds = [
    ...(PRESENT.has(mode) ? [[`swarm restart ${a.id}`, "recycle"], [`swarm logs ${a.id} -f`, "tail logs"], [`swarm stop ${a.id}`, "send home"]] : [[`swarm start ${a.id}`, "bring in"]]),
    [`swarm learn ${a.id}`, "nap + dream"],
    ...(a.pendingLearnings ? [[`swarm learn list ${a.id}`, "review lessons"]] : []),
    ...(a.pendingSkills ? [[`swarm learn skills ${a.id}`, "review skills"]] : []),
  ];
  return `<div style="--hue:${hueOf(a.unit)};--ring:${MODE[mode].color}">
    <div class="who">
      <div class="portrait">${faceHTML(a)}</div>
      <div><h2>${esc(a.name)}</h2>
        <div class="chips">
          <span class="chip" style="--c:${MODE[mode].color}"><span class="pip"></span>${MODE[mode].label}</span>
          <span class="chip" style="--c:${hueOf(a.unit)}">${esc(shortName(unitName(a.unit)))}</span>
        </div></div>
    </div>
    <p class="role">${esc(a.role)}</p>
    <div class="bars">
      <div class="tile gold"><b>${review(a)}</b><span>to review</span></div>
      <div class="tile"><b>${compact(u?.requests ?? 0)}</b><span>LLM calls</span></div>
      <div class="tile"><b>${compact((u?.inputTokens ?? 0) + (u?.outputTokens ?? 0))}</b><span>tokens</span></div>
    </div>
    <h3>Now</h3>
    <dl class="stats">
      <dt>Doing</dt><dd>${esc(statusText(a, mode, people.get(a.id)))}</dd>
      ${a.live?.tool ? `<dt>Tool</dt><dd>${esc(toolLabel(a.live.tool))} <small>for ${ago(a.live.toolSince).replace(/ ago$/, "")}${a.live.running > 1 ? ` · ${a.live.running} calls running` : ""}</small></dd>` : ""}
      ${a.live?.waiting ? `<dt>Waiting</dt><dd class="warn">${a.live.waiting} approval${a.live.waiting === 1 ? "" : "s"} for a person</dd>` : ""}
      ${a.activity ? `<dt>Engine</dt><dd>${a.activity.activeAgents} turn${a.activity.activeAgents === 1 ? "" : "s"} running${a.activity.gatewayBusy ? " · gateway busy" : ""}</dd>
      ${a.activity.runningJobs?.length ? `<dt>Scheduled</dt><dd>${a.activity.runningJobs.map(esc).join(", ")} <small>running</small></dd>` : ""}
      <dt>Last active</dt><dd>${ago(a.activity.lastActive)}${a.activity.source ? ` <small>on ${esc(sourceLabel(a.activity.source))}</small>` : ""}</dd>
      ${a.activity.lastJobAt ? `<dt>Last job</dt><dd>${ago(a.activity.lastJobAt)} <small>scheduled</small></dd>` : ""}
      <dt>Platforms</dt><dd>${Object.entries(a.activity.platforms).map(([k, v]) => `${esc(k.replace(/_/g, " "))} <small${v.needsAttention ? ' class="warn"' : ""}>${esc(v.state)}</small>`).join(", ") || "none"}</dd>`
        : `<dt>Engine</dt><dd><small>${PRESENT.has(mode) ? "its API is not answering yet" : "not running"}</small></dd>`}
    </dl>
    ${timelineHTML(a)}
    <h3>Status</h3>
    <dl class="stats">
      <dt>Container</dt><dd>${esc(a.state)}${a.health ? ` <small>(${esc(a.health)})</small>` : ""}</dd>
      ${a.detail ? `<dt>Note</dt><dd class="warn">${esc(a.detail)}</dd>` : ""}
      <dt>Last nap</dt><dd${stale ? ' class="warn"' : ""}>${ago(a.lastNap)}${stale ? " · overdue" : ""} <small>every ${Math.round(a.napIntervalSeconds / 60)} min</small></dd>
      <dt>Learnings</dt><dd>${a.pendingLearnings} proposed · ${a.pendingSkills} skill${a.pendingSkills === 1 ? "" : "s"}</dd>
      <dt>API</dt><dd>${esc(a.endpoint)}</dd>
      <dt>Channels</dt><dd>${a.channels.map((c) => (a.optionalChannels?.includes(c) ? `${esc(c)} <small>(only with its token)</small>` : esc(c))).join(", ") || "none"}</dd>
    </dl>
    <h3>Workstation</h3>
    <div class="desk-list">${[...new Set(deskOf(a).apps)].map((x) => `${x} screen${deskOf(a).apps.filter((y) => y === x).length > 1 ? " ×2" : ""}`).concat(deskOf(a).props, deskOf(a).side).map((x) => `<span>${esc(x.replace(/-/g, " "))}</span>`).join("")}</div>
    <p class="note">${a.desk ? "From its persona's <code>desk:</code> block." : `${esc(shortName(unitName(a.unit)))} defaults; set <code>desk:</code> in its persona to change them.`}</p>
    <h3>Model</h3>
    <dl class="stats">
      ${a.localModel ? `<dt>Here</dt><dd>${esc(a.localModel)} <small>via core, ChatGPT plan</small></dd><dt>Fallback</dt><dd>${esc(a.model)}</dd>` : `<dt>Model</dt><dd>${esc(a.model)}</dd>`}
      ${u ? `<dt>Through core</dt><dd>${u.ok} ok · ${u.rateLimited} limited · ${u.errors} failed</dd>
      <dt>Tokens</dt><dd>${compact(u.inputTokens)} in <small>(${compact(u.cachedTokens)} cached)</small> · ${compact(u.outputTokens)} out</dd>
      <dt>Last call</dt><dd>${ago(u.lastAt)}</dd>` : ""}
    </dl>
    <h3>Commands</h3>
    <div class="cmds">${cmds.map(([c, h]) => cmdButton(c, h)).join("")}</div>
    <p class="note">Click to copy, then run it in your terminal. The office is read-only for now.</p>
  </div>`;
}

// The selected agent's recent hooks (tool names and times only; previews stay with the owner's CLI).
const timelines = new Map(); // agent id → entries
const EVENT = {
  pre_tool_call: "started",
  post_tool_call: "finished",
  on_session_start: "session opened",
  on_session_end: "session closed",
  pre_approval_request: "asked for approval",
  post_approval_response: "approval answered",
};

async function fetchTimeline(id) {
  try {
    const r = await fetch(`/api/agents/${encodeURIComponent(id)}/timeline?limit=14`, { cache: "no-store" });
    if (r.ok) timelines.set(id, (await r.json()).entries ?? []);
  } catch {
    /* the panel keeps the last list */
  }
}

function timelineHTML(a) {
  const rows = timelines.get(a.id);
  if (!rows?.length) return a.live ? "" : `<h3>Activity</h3><p class="note">No hook events yet: they arrive once the agent runs with the core's <code>hooks.outbound</code> (restart it after <code>stormo check</code>).</p>`;
  return `<h3>Activity</h3><ol class="timeline">${rows
    .map((e) => `<li><time>${esc(ago(e.at))}</time> ${e.tool ? `<b>${esc(toolLabel(e.tool))}</b> ` : ""}${esc(EVENT[e.event] ?? e.event)}</li>`)
    .join("")}</ol><p class="note">Details (files, commands) stay in the core: <code>stormo core activity ${esc(a.id)}</code>.</p>`;
}

function corePanel() {
  const gw = snap.gateway;
  const limited = !!gw.planLimitedUntil;
  const color = limited ? "var(--bad)" : gw.login === "ok" ? "var(--ok)" : "var(--warn)";
  const rows = Object.entries(gw.usage ?? {});
  const name = (id) => snap.agents.find((a) => a.id === id)?.name ?? id;
  return `<div style="--ring:${color};--hue:#34d399">
    <div class="who">
      <div class="portrait" aria-hidden="true">⌁</div>
      <div><h2>Swarm core</h2>
        <div class="chips"><span class="chip" style="--c:${color}"><span class="pip"></span>${limited ? "Plan limit" : gw.login === "ok" ? "Using ChatGPT plan" : esc(gw.login)}</span></div></div>
    </div>
    <p class="role">${gw.login === "ok" ? "Using ChatGPT plan. " : ""}Holds the one ChatGPT sign-in and serves it to every local agent. Agents fall back to their own provider when it is down or limited.</p>
    <div class="bars">
      <div class="tile"><b>${gw.inflight}/${gw.concurrency}</b><span>in flight</span></div>
      <div class="tile"><b>${gw.queued}</b><span>queued</span></div>
      <div class="tile"><b>${rows.length}</b><span>callers</span></div>
    </div>
    <h3>Gateway</h3>
    <dl class="stats">
      <dt>Login</dt><dd>${esc(gw.login)}</dd>
      <dt>Plan limit</dt><dd${limited ? ' class="warn"' : ""}>${limited ? `until ${clockTime(new Date(gw.planLimitedUntil))}` : "clear"}</dd>
      <dt>Fleet poll</dt><dd>${ago(snap.polledAt)}</dd>
    </dl>
    <h3>Usage since start</h3>
    ${rows.length ? `<table class="usage"><tr><th>Agent</th><th>Calls</th><th>429</th><th>Tokens</th><th>Last</th></tr>
      ${rows.map(([id, u]) => `<tr><td>${esc(name(id))}</td><td>${u.requests}</td><td>${u.rateLimited}</td><td>${compact(u.inputTokens + u.outputTokens)}</td><td>${ago(u.lastAt)}</td></tr>`).join("")}</table>`
      : '<p class="note">No model calls yet.</p>'}
    ${gw.manageUsageUrl && gw.login === "ok" ? `<a class="usage-link${limited ? " primary" : ""}" href="${esc(gw.manageUsageUrl)}" target="_blank" rel="noopener noreferrer">Manage usage <span aria-hidden="true">↗</span></a>` : ""}
    <h3>Commands</h3>
    <div class="cmds">${gw.login !== "ok" ? cmdButton("swarm core login", "sign in") : ""}${cmdButton("swarm core status", "usage")}</div>
  </div>`;
}

function vacantPanel(unit) {
  return `<div style="--hue:${hueOf(unit)};--ring:var(--off)">
    <div class="who"><div class="portrait" aria-hidden="true">+</div>
      <div><h2>Open desk</h2><div class="chips"><span class="chip" style="--c:${hueOf(unit)}">${esc(shortName(unitName(unit)))}</span></div></div></div>
    <p class="role">Nobody works here yet. A desk is filled by an agent manifest with <code>unit: ${esc(unit)}</code>.</p>
    <h3>Hire</h3>
    <dl class="stats">
      <dt>Manifest</dt><dd><code>agents/&lt;id&gt;/agent.yaml</code></dd>
      <dt>Behavior</dt><dd><code>agents/&lt;id&gt;/SOUL.md</code></dd>
      <dt>Look</dt><dd><code>personas/&lt;slug&gt;/</code> (persona.md)</dd>
    </dl>
    <h3>Commands</h3>
    <div class="cmds">${cmdButton("swarm check", "validate")}</div>
  </div>`;
}

function renderInspector() {
  if (!selected || !snap) {
    inspector.hidden = true;
    return;
  }
  const a = selected.kind === "agent" && snap.agents.find((x) => x.id === selected.id);
  if (selected.kind === "agent" && !a) return close();
  const html = a ? agentPanel(a) : selected.kind === "core" ? corePanel() : selected.kind === "robot" && snap.robot ? robotPanel() : vacantPanel(selected.unit);
  if (body.dataset.html !== html) {
    body.innerHTML = html;
    body.dataset.html = html;
  }
  inspector.hidden = false;
}

function select(sel) {
  const same = sel && selected && sel.kind === selected.kind && sel.id === selected.id && sel.unit === selected.unit;
  selected = same ? null : sel;
  body.dataset.html = "";
  refresh();
}

function close() {
  selected = null;
  inspector.hidden = true;
  refresh();
}

// ---------- loop ----------

function refresh() {
  if (!snap) return;
  for (const a of snap.agents) updateAgent(a, snap.gateway);
  updateGateway(snap.gateway);
  followRobot(snap.robot);
  meters();
  renderInspector();
  drawLinks();
}

async function poll() {
  try {
    const r = await fetch("/api/fleet", { cache: "no-store" });
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const next = await r.json();
    if (typeof next.now === "number") clockOffset = next.now - Date.now();
    const key = JSON.stringify([next.units.map((u) => u.id), next.agents.map((a) => [a.id, a.unit, a.name, a.avatar]), !!next.robot]);
    snap = next;
    if (selected?.kind === "agent") await fetchTimeline(selected.id);
    if (key !== rosterKey) {
      rosterKey = key;
      build();
    }
    refresh();
    if (failures) feed("<b>Core</b> is back");
    failures = 0;
    $("#toast").hidden = true;
    if (firstPaint) welcome();
  } catch {
    failures++;
    const t = $("#toast");
    t.textContent = "Can't reach the swarm core. Retrying… (start it with: swarm core up)";
    t.hidden = failures < 2 && !firstPaint;
  }
}

function welcome() {
  firstPaint = false;
  const on = snap.agents.filter((a) => PRESENT.has(modeOf(a, snap.gateway)) && modeOf(a, snap.gateway) !== "unknown").length;
  const r = snap.agents.reduce((n, a) => n + review(a), 0);
  const n = snap.agents.length;
  $("#welcome-sub").textContent = n
    ? `${on} of ${n} agent${n === 1 ? "" : "s"} at ${n === 1 ? "their desk" : "their desks"} · ${r} waiting for review`
    : "No agents yet. Add one under agents/<id>/agent.yaml.";
  feed(`Office opened · ${on}/${n} on shift`);
  setTimeout(() => $("#welcome").classList.add("gone"), 2600);
}

document.addEventListener("click", (e) => {
  const copy = e.target.closest("[data-copy]");
  if (copy) {
    navigator.clipboard?.writeText(copy.dataset.copy).then(
      () => feed(`Copied <b>${esc(copy.dataset.copy)}</b>`),
      () => feed(`Run <b>${esc(copy.dataset.copy)}</b>`),
    );
    return;
  }
  const t = e.target.closest("[data-select], [data-vacant], #rack, #robot, #close");
  if (!t) return;
  if (t.id === "close") return close();
  if (t.id === "robot") return select({ kind: "robot" });
  if (t.id === "rack") return select({ kind: "core" });
  if (t.dataset.vacant) return select({ kind: "vacant", unit: t.dataset.vacant });
  select({ kind: "agent", id: t.dataset.select });
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && selected) close();
});
// A new layout moves every chair: everyone sits back down where they are now.
let resizeTimer;
window.addEventListener("resize", () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(resync, 150);
});
document.addEventListener("visibilitychange", () => !document.hidden && resync());

function tick() {
  $("#clock").textContent = new Date().toLocaleTimeString([], { hour12: false });
  // World clocks on the core's time, so every viewer's wall agrees.
  const now = new Date(serverNow());
  for (const el of floor.querySelectorAll(".clock-tile")) {
    const c = clockFormats[Number(el.dataset.clock)];
    if (!c) continue;
    const [h, m] = c.hm.format(now).split(":");
    const time = $(".clk-time", el);
    const html = `${h}<span class="colon">:</span>${m}`;
    if (time.innerHTML !== html) time.innerHTML = html;
    el.classList.toggle("night", Number(h) < 7 || Number(h) >= 19);
    el.title = c.full.format(now);
    el.setAttribute("aria-label", `${c.label}: ${h}:${m}`);
  }
}

tick();
setInterval(tick, 1000);
poll();
setInterval(poll, POLL_MS);
