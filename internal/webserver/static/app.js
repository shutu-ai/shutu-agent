/* Shutu AI Agent — dsh-style workspace (P1). Vanilla JS, no build, zero
   dependencies. Ported UI conventions from dsh web (ui-layout / ui-conversation
   / ui-theme). Auth is optional (D-WEB2-G): no token configured → the portal
   serves open; a 401 drops to the login view. */

"use strict";

// ---- storage keys -------------------------------------------------------
const KEY_TOKEN = "pa_token";
const KEY_THEME = "pa_theme";
const KEY_CURRENT = "pa_current";
const KEY_RECENT_WS = "pa_recent_workspace"; // dsh recentWorkspaceId: where 新会话 lands

// ---- layout constants (ui-layout columns.ts) -----------------------------
const SIDEBAR_DEFAULT = 280;
const SIDEBAR_MIN = 264;
const SIDEBAR_MAX = 420;
const SIDEBAR_COLLAPSED = 56;
const SIDEBAR_AUTO_COLLAPSE = 1024;
const CENTER_MIN = 640;
const DETAILS_DEFAULT = 360;
const DETAILS_MIN = 300;
const DETAILS_MAX = 520;

// --- mode presets (== shutu-agent config mode, dsh agent preset) -------------
const HERO_MODES = [
  { id: "standard", name: "标准模式", desc: "完整的编码 agent。" },
  { id: "code", name: "PTC 模式", desc: "标准模式全部能力 + 程序化操作（Code Mode）。" },
  { id: "minimal", name: "极简模式", desc: "只保留持久终端 + 文件编辑。" },
];
const MODE_DISPLAY = { minimal: "极简模式", standard: "标准模式", code: "PTC 模式" };
const PERMISSION_NAMES = { readonly: "只读", standard: "标准", full: "全部" };

// ---- element refs --------------------------------------------------------
const $ = (id) => document.getElementById(id);
const loginEl = $("login"), loginForm = $("login-form"), loginMsg = $("login-msg");
const workspaceEl = $("workspace"), frameEl = $("frame");
const sessionList = $("session-list"), newSessionBtn = $("new-session");
const curSessionEl = $("cur-session"), modeBadgeEl = $("mode-badge");
const topbarEl = $("topbar"); // hidden while the session is blank (dsh headerHidden)
const messagesEl = $("messages"), heroEl = $("hero");
const colCenterEl = document.querySelector(".col-center");
const composerText = $("composer-text"), composerBox = $("composer"), sendBtn = $("composer-send");
const slashMenu = $("slash-menu");
const growWrapEl = document.querySelector(".grow-wrap");
const scrollBottomBtn = $("scroll-bottom");
const settingsEl = $("settings"), placeholderEl = $("placeholder");
const heroWsChip = $("hero-ws-chip"), heroWsLabel = $("hero-ws-label"), heroWsMenu = $("hero-ws-menu");
const heroModeChip = $("hero-mode-chip"), heroModeLabel = $("hero-mode-label"), heroModeMenu = $("hero-mode-menu");
const cmdBtn = $("cmd-btn"), cmdMenu = $("cmd-menu");
const permSeat = $("perm-seat"), permSeatLabel = $("perm-seat-label"), permSeatIcon = $("perm-seat-icon"), permMenu = $("perm-menu");
const modelSeat = $("model-seat"), modelSeatLabel = $("model-seat-label"), modelMenu = $("model-menu");
const contextMeter = $("context-meter");
const detailsPanel = $("details-panel"), detailsTitle = $("details-title"),
  detailsCloseBtn = $("details-close"), detailsEmptyEl = $("details-empty"), detailsSelEl = $("details-selection");

// ---- state ---------------------------------------------------------------
let currentID = localStorage.getItem(KEY_CURRENT) || "";
let sessionEmpty = !currentID;        // the current session has no events yet (blank hero)
let heroActive = true;                // hero phase (centered composer) vs active phase (docked)
let layout = { sidebar: SIDEBAR_DEFAULT, manual: false, narrowViewport: false, dragging: false };
let details = { open: false, width: DETAILS_DEFAULT }; // right details column (dsh layout details)
let heroWorkspace = "";             // selected hero workspace id ("" = pick a workspace)
let heroMenuOpen = false;           // hero workspace picker popover state
let heroModeOpen = false;           // hero mode (agent preset) popover state
let cmdMenuOpen = false;            // composer +(command) menu popover state
let slashMenuOpen = false;          // leading-/ command suggestions in the composer
let slashHighlight = 0;
let modelMenuOpen = false;          // composer model seat popover state
let modelPane = "root";             // model seat menu pane: root | model | effort (dsh ModelSelect)
let effortTarget = "";              // model id whose effort pane is open ("" = current model)
let effortTargetProv = "";          // provider id of effortTarget
let modelSearch = "";               // live search term in the model pane (kept across re-renders)
let catalogLoading = false;         // /api/config refresh in flight (dsh status.loading)
let catalogError = null;            // last /api/config refresh error (dsh error strip + 重新加载)
let modelBusy = false;              // a selection is being applied (dsh busy disables the items)
let mode = "";                      // current mode preset: standard | code | minimal
let permissionPreset = "";          // current permission preset: readonly | standard | full
let permMenuOpen = false;           // composer permission seat popover state
let sessionCfg = { provider: "", model: "", effort: "", permission: "" }; // active session's per-session selection (dsh ModelSelection: provider+model+effort; "" → fall back global)
let wsList = [];                    // [{id,title}] for the hero picker (from /api/workspaces)
let toolMeta = {};                  // callId -> {name, args} captured from assistant tool_call
let selectedTool = null;            // {callId,name,args,output,error} shown in the details panel
let sseAbort = null;            // AbortController for the current session stream
let sseReconnect = null;        // timer handle
let streamState = null;         // {seq, node} for the assistant bubble being built
let reasoningLive = false;      // thinking deltas of the current step already streamed
let currentReasoningNode = null; // the live Think row being accumulated
let lastReasoningSeq = 0;       // seq of the last rendered reasoning delta (step boundary)
let renderedSeqs = new Set();   // event seqs rendered in the current view (replay dedup)
let feedbackBySeq = new Map();  // assistant event seq -> positive | negative
let runningNode = null;         // "Deep diving..." element (turn-level, dsh TurnStatus)
let runningStart = 0;           // wall clock of the running turn's start (elapsed anchor)
let runningTimer = null;        // 1s ticker for the elapsed clock
let pollTimer = null;           // session-list refresh
let config = {};                // cached GET /api/config view
let pendingSlashCommands = [];  // built-in slash inputs waiting for their result

// The web command catalog is loaded from the backend config.commands response.
// The server remains authoritative and still handles unknown commands. The
// built-in fallback makes the trigger useful during the first async config
// load; a successful response replaces it and appends the skill entries.
const DEFAULT_WEB_COMMANDS = [
  { name: "help", hint: "Show available slash commands", kind: "command" },
  { name: "status", hint: "Show provider, model and mode", kind: "command" },
  { name: "compact", hint: "Compact context: /compact [region start end]", kind: "command" },
  { name: "permission", hint: "Show or set permission: /permission [readonly|standard|full]", kind: "command" },
  { name: "feedback", hint: "Record feedback: /feedback <text>", kind: "command" },
  { name: "goal", hint: "Manage the goal: /goal [objective|clear|edit <objective>|pause|resume]", kind: "command" },
  { name: "plan", hint: "Plan mode: /plan [off|message]", kind: "command" },
  { name: "export", hint: "Download Session log: /export", kind: "command" },
];
let webCommands = DEFAULT_WEB_COMMANDS.slice();

// noteRendered records one rendered event seq. A Set — not a watermark — so a
// gap event (dropped by the SSE hub) stays "not rendered" even when later
// events advanced past it, and the post-turn reconcile can still repair it.
const MAX_RENDERED_SEQS = 4000;
function noteRendered(seq) {
  if (seq == null) return;
  renderedSeqs.add(seq);
  if (renderedSeqs.size > MAX_RENDERED_SEQS) {
    const oldest = [...renderedSeqs].sort((a, b) => a - b);
    const cut = oldest.length - MAX_RENDERED_SEQS;
    for (let i = 0; i < cut; i++) renderedSeqs.delete(oldest[i]);
  }
}

// ---- token / api ---------------------------------------------------------
function token() { return localStorage.getItem(KEY_TOKEN) || ""; }

// api performs an authenticated JSON request; a 401 drops to the login view.
async function api(path, opts = {}) {
  const headers = Object.assign({ "Content-Type": "application/json" }, opts.headers || {});
  if (token()) headers.Authorization = "Bearer " + token();
  const res = await fetch(path, Object.assign({}, opts, { headers }));
  if (res.status === 401) { showLogin("令牌无效或已过期"); throw new Error("unauthorized"); }
  return res;
}

// ---- login ---------------------------------------------------------------
function showLogin(msg) {
  loginMsg.textContent = msg || "";
  loginMsg.classList.toggle("hidden", !msg);
  loginEl.classList.remove("hidden");
}
function hideLogin() { loginEl.classList.add("hidden"); }

// ---- theme (P5: light / dark / system, dsh ThemeRuntime) ---------------------
function currentDark() {
  const pref = localStorage.getItem(KEY_THEME) || "system";
  if (pref === "light") return false;
  if (pref === "dark") return true;
  return !!(window.matchMedia && matchMedia("(prefers-color-scheme: dark)").matches);
}
function applyTheme() {
  const dark = currentDark();
  document.documentElement.style.colorScheme = dark ? "dark" : "light";
  document.body.setAttribute("data-ds-dark-theme", dark ? "true" : "false");
  let meta = document.querySelector('meta[name="theme-color"]');
  if (!meta) { meta = document.createElement("meta"); meta.name = "theme-color"; document.head.appendChild(meta); }
  meta.content = dark ? "#151517" : "#FFFFFF";
  // Brand logo: the user's monochrome PNG — white mark on dark, black on light.
  // The rail toggle's brand mark follows the same theme (dsh brand mark slot).
  const logo = $("brand-logo");
  if (logo) logo.src = dark ? "/static/big_logo_2.png" : "/static/big_logo_1.png";
  const hlogo = $("hero-logo");
  if (hlogo) hlogo.src = dark ? "/static/big_logo_2.png" : "/static/big_logo_1.png";
  const tlogo = $("toggle-logo");
  if (tlogo) tlogo.src = dark ? "/static/big_logo_2.png" : "/static/big_logo_1.png";
  const icon = $("theme-toggle");
  if (icon) icon.innerHTML = dark ? DSH_ICON_LIGHT : DSH_ICON_DARK;
  const icon2 = $("theme-toggle-settings");
  if (icon2) icon2.innerHTML = dark ? DSH_ICON_LIGHT : DSH_ICON_DARK;
}
function toggleTheme() {
  const dark = currentDark();
  localStorage.setItem(KEY_THEME, dark ? "light" : "dark");
  applyTheme();
}
function initThemeSystem() {
  if (!window.matchMedia) return;
  matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
    // system follows the OS; a manual preference short-circuits (dsh ThemeRuntime)
    if ((localStorage.getItem(KEY_THEME) || "system") === "system") applyTheme();
  });
}

// ---- layout: frame grid + drag + narrow (dsh ui-layout columns + the
//      sidebar toggle from the logo row) -------------------------------------
// The rail is reached either automatically (viewport < SIDEBAR_AUTO_COLLAPSE)
// or by the manual panel toggle; auto-collapse is the only force when the
// viewport is narrow, manual is the only force otherwise.
function sidebarCollapsed() { return layout.narrowViewport || layout.manual; }
function renderColumns() {
  const collapsed = sidebarCollapsed();
  const detailsW = details.open ? details.width : 0;
  frameEl.style.gridTemplateColumns =
    (collapsed ? SIDEBAR_COLLAPSED : layout.sidebar) + "px minmax(0, 1fr) " + detailsW + "px";
  frameEl.dataset.sidebarCollapsed = String(collapsed);
  frameEl.dataset.detailsCollapsed = String(detailsW === 0);
  const h = document.querySelector(".drag-handle");
  if (h) h.style.left = (collapsed ? SIDEBAR_COLLAPSED : layout.sidebar) + "px";
  const dh = document.querySelector(".drag-handle-details");
  if (dh) {
    dh.style.display = details.open ? "" : "none";
    dh.style.right = detailsW + "px";
  }
}
function clampDetails(v) { return Math.max(DETAILS_MIN, Math.min(DETAILS_MAX, v)); }
function clampSidebar(v) { return Math.max(SIDEBAR_MIN, Math.min(SIDEBAR_MAX, v)); }
function syncSidebarToggle() {
  const collapsed = sidebarCollapsed();
  const t = $("sidebar-toggle");
  if (t) t.title = collapsed ? "展开侧栏" : "折叠侧栏";
  const b = $("brand");
  if (b) b.title = collapsed ? "展开侧栏" : "新建会话";
}
function toggleSidebar() {
  layout.manual = !sidebarCollapsed();
  renderColumns();
  syncSidebarToggle();
}

function setupDrag() {
  const handle = document.createElement("div");
  handle.className = "drag-handle";
  handle.dataset.side = "sidebar";
  frameEl.appendChild(handle);
  handle.style.left = (sidebarCollapsed() ? SIDEBAR_COLLAPSED : layout.sidebar) + "px";

  let origin = 0, base = layout.sidebar, frame = null;
  handle.addEventListener("pointerdown", (e) => {
    if (sidebarCollapsed()) return; // no handle while collapsed
    e.preventDefault();
    handle.setPointerCapture(e.pointerId);
    origin = e.clientX;
    base = layout.sidebar;
    layout.dragging = true;
    frameEl.dataset.dragging = "true";
    handle.dataset.dragging = "true";
  });
  handle.addEventListener("pointermove", (e) => {
    if (!handle.hasPointerCapture(e.pointerId)) return;
    frame ??= requestAnimationFrame(() => {
      frame = null;
      layout.sidebar = clampSidebar(base + (e.clientX - origin));
      renderColumns();
    });
  });
  const end = () => {
    if (!handle.hasPointerCapture(handle.pointerId)) return;
    handle.releasePointerCapture(handle.pointerId);
    if (frame) { cancelAnimationFrame(frame); frame = null; }
    layout.dragging = false;
    delete frameEl.dataset.dragging;
    delete handle.dataset.dragging;
  };
  handle.addEventListener("pointerup", end);
  handle.addEventListener("pointercancel", end);

  // Details-column resize handle (right edge; only while the panel is open).
  const dhandle = document.createElement("div");
  dhandle.className = "drag-handle drag-handle-details";
  dhandle.dataset.side = "details";
  frameEl.appendChild(dhandle);
  dhandle.style.right = (details.open ? details.width : 0) + "px";

  let dorigin = 0, dbase = details.width, dframe = null;
  dhandle.addEventListener("pointerdown", (e) => {
    if (!details.open) return;
    e.preventDefault();
    dhandle.setPointerCapture(e.pointerId);
    dorigin = e.clientX;
    dbase = details.width;
    layout.dragging = true;
    frameEl.dataset.dragging = "true";
    dhandle.dataset.dragging = "true";
  });
  dhandle.addEventListener("pointermove", (e) => {
    if (!dhandle.hasPointerCapture(e.pointerId)) return;
    dframe ??= requestAnimationFrame(() => {
      dframe = null;
      details.width = clampDetails(dbase - (e.clientX - dorigin));
      renderColumns();
    });
  });
  const dend = () => {
    if (!dhandle.hasPointerCapture(dhandle.pointerId)) return;
    dhandle.releasePointerCapture(dhandle.pointerId);
    if (dframe) { cancelAnimationFrame(dframe); dframe = null; }
    layout.dragging = false;
    delete frameEl.dataset.dragging;
    delete dhandle.dataset.dragging;
  };
  dhandle.addEventListener("pointerup", dend);
  dhandle.addEventListener("pointercancel", dend);
}

function setupNarrow() {
  const ro = new ResizeObserver(() => {
    const w = frameEl.clientWidth;
    layout.narrowViewport = w < SIDEBAR_AUTO_COLLAPSE;
    renderColumns();
    syncSidebarToggle();
  });
  ro.observe(frameEl);
}

// ---- utilities ------------------------------------------------------------
function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function fmtTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const now = new Date();
  const pad = (n) => String(n).padStart(2, "0");
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (d.toDateString() === now.toDateString()) return hm;
  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}月${d.getDate()}日 ${hm}`;
  return `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日 ${hm}`;
}
function msgInner() {
  let inner = messagesEl.querySelector(".messages-inner");
  if (!inner) {
    inner = document.createElement("div");
    inner.className = "messages-inner";
    messagesEl.appendChild(inner);
  }
  return inner;
}

// ---- lightweight markdown -------------------------------------------------
// Renders escaped text into VALID, compact HTML (dsh display scheme): fenced
// code blocks stay intact; consecutive "- "/"1. " lines merge into ONE
// ul/ol (never wrapped in a <p>, which inflated the gaps between items);
// headings and paragraphs are separate block elements with tight spacing.
// GFM tables ("| a | b |" + "|---|---|" delimiter row, optional ":" column
// alignment) render as real <table> markup, like the dsh markdown renderer.
function renderMarkdown(text) {
  const t = esc(text);
  const parts = t.split(/(```[\s\S]*?```)/g);
  const out = [];
  const inline = (s) => s
    .replace(/`([^`\n]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
  // A table row starts with "|" and holds at least two cells ("| a | b |");
  // the delimiter row contains only pipes, dashes, colons and spaces.
  const isTableRow = (line) => /^\s*\|/.test(line) && line.split("|").length >= 3;
  const isDelimRow = (line) => /^\s*\|?[\s|:-]+\|?\s*$/.test(line) && line.includes("|") && line.includes("-");
  // Split one table row into trimmed cells; "\|" is an escaped literal pipe.
  const splitCells = (line) => {
    const escPipe = "\u0000";
    let s = line.trim().replace(/\\\|/g, escPipe);
    if (s.startsWith("|")) s = s.slice(1);
    if (s.endsWith("|")) s = s.slice(0, -1);
    return s.split("|").map((c) => c.trim().replace(/\u0000/g, "|"));
  };
  // Delimiter-cell alignment: ":---" left, "---:" right, ":---:" center.
  const cellAlign = (cell) => {
    if (!/^:?-+:?$/.test(cell.trim())) return null;
    if (cell.startsWith(":") && cell.endsWith(":")) return "center";
    if (cell.endsWith(":")) return "right";
    if (cell.startsWith(":")) return "left";
    return null;
  };
  const renderTable = (headCells, delimCells, rows) => {
    const align = delimCells.map(cellAlign);
    const cols = headCells.length;
    const st = (i) => (align[i] ? ` style="text-align:${align[i]}"` : "");
    let h = `<div class="md-table"><table><thead><tr>`;
    for (let i = 0; i < cols; i++) h += `<th${st(i)}>${inline(headCells[i] || "")}</th>`;
    h += `</tr></thead><tbody>`;
    for (const row of rows) {
      h += "<tr>";
      for (let i = 0; i < cols; i++) h += `<td${st(i)}>${inline(row[i] || "")}</td>`;
      h += "</tr>";
    }
    h += "</tbody></table></div>";
    return h;
  };
  for (const p of parts) {
    if (/^```/.test(p)) {
      const code = p.replace(/^```[^\n]*\n?/, "").replace(/```$/, "");
      out.push(`<pre><code>${code}</code></pre>`);
      continue;
    }
    if (!p) continue;
    let html = "";
    let listTag = null; // "ul" | "ol" while inside a list
    let para = [];
    let pendingHead = null;  // a "| a | b |" line awaiting its delimiter row
    let tblHead = null;      // collecting a table once the delimiter matched
    let tblAlign = null;
    let tblRows = null;
    const flushPara = () => {
      if (para.length > 0) {
        html += `<p>${inline(para.join("\n"))}</p>`;
        para = [];
      }
    };
    const flushList = () => {
      if (listTag !== null) { html += `</${listTag}>`; listTag = null; }
    };
    const closeTable = () => {
      if (tblHead !== null) {
        html += renderTable(tblHead, tblAlign, tblRows);
        tblHead = null; tblAlign = null; tblRows = null;
      }
    };
    for (const raw of p.split("\n")) {
      const line = raw.trimEnd();
      if (tblHead !== null) {
        // A stray delimiter row inside the body means the table ended; the
        // line then falls through as plain text (and may open a new table).
        if (isDelimRow(line)) { closeTable(); }
        else if (isTableRow(line)) { tblRows.push(splitCells(line)); continue; }
        else { closeTable(); }
      }
      if (pendingHead !== null) {
        if (isDelimRow(line)) {
          tblHead = splitCells(pendingHead);
          tblAlign = splitCells(line);
          tblRows = [];
          pendingHead = null;
          continue;
        }
        // not a table after all: the header line is plain text
        para.push(pendingHead);
        pendingHead = null;
      }
      const ul = /^[-*] (.+)$/.exec(line);
      const ol = /^\d+\. (.+)$/.exec(line);
      const head = /^(#{1,4}) (.+)$/.exec(line);
      if (isTableRow(line)) {
        flushPara();
        flushList();
        pendingHead = line;
        continue;
      }
      if (ul || ol) {
        const tag = ul ? "ul" : "ol";
        flushPara();
        if (listTag !== tag) { flushList(); html += `<${tag}>`; listTag = tag; }
        html += `<li>${inline((ul || ol)[1])}</li>`;
        continue;
      }
      flushList();
      if (head) {
        flushPara();
        html += `<h3>${inline(head[2])}</h3>`;
        continue;
      }
      if (line.trim() === "") { flushPara(); continue; }
      para.push(line);
    }
    closeTable();
    if (pendingHead !== null) para.push(pendingHead);
    flushPara();
    flushList();
    out.push(html);
  }
  const buf = out.join("");
  return buf || esc(text);
}

// ---- dsh tool-row presentation (labels / icons / Think streaming) --------
// Glyphs extracted from @deepseek-ai/dsh-client-ui-primitives (ic_ds_* set);
// every glyph renders at 14px inside the 16px leading slot.
const DSH_ICON_SEARCH = '<svg width="14" height="14" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"> <path d="M11.894845 6.647401C11.894845 3.725463 9.534486 1.356779 6.623219 1.35657C3.711786 1.35657 1.351635 3.725338 1.351635 6.647401C1.351843 9.569296 3.711911 11.938273 6.623219 11.938273C9.534361 11.938064 11.894637 9.569171 11.894845 6.647401ZM13.245462 6.647401C13.245254 10.317935 10.280401 13.293613 6.623219 13.293821C2.965871 13.293821 0.000204 10.31806 0 6.647401C0 2.976574 2.965746 0 6.623219 0C10.280526 0.000205 13.245462 2.9767 13.245462 6.647401Z" fill="currentColor" /> <path d="M16.000417 15.041079L15.044449 16.000433L11.530434 12.473588L12.486298 11.514234L16.000417 15.041079Z" fill="currentColor" /> </svg>';
const DSH_ICON_BROWSE = '<svg width="14" height="14" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"> <path d="M11.2426 4.80473V6.10551H4.75819V4.80473H11.2426Z" fill="currentColor" /> <path d="M9.40858 7.84478V9.14557H4.75819V7.84478H9.40858Z" fill="currentColor" /> <path d="M9.23438 0.546389C10.1941 0.546389 10.9683 0.544914 11.5859 0.611819C12.2161 0.680096 12.7634 0.825745 13.2393 1.17139C13.5172 1.3733 13.7619 1.61812 13.9639 1.896C14.3096 2.37183 14.4551 2.91922 14.5234 3.54932C14.5903 4.16686 14.5889 4.94133 14.5889 5.90088V10.0981C14.5889 11.0576 14.5903 11.8321 14.5234 12.4497C14.4552 13.0798 14.3094 13.6272 13.9639 14.103C13.7619 14.381 13.5172 14.6257 13.2393 14.8276C12.7633 15.1734 12.2163 15.3189 11.5859 15.3872C10.9683 15.4541 10.1942 15.4536 9.23438 15.4536H6.76563C5.80591 15.4536 5.03168 15.4541 4.41407 15.3872C3.78385 15.3189 3.23665 15.1734 2.76074 14.8276C2.48291 14.6257 2.23802 14.3809 2.03614 14.103C1.69066 13.6272 1.54483 13.0798 1.47657 12.4497C1.40973 11.8321 1.41114 11.0576 1.41114 10.0981V5.90088C1.41113 4.94132 1.40966 4.16686 1.47657 3.54932C1.54488 2.91921 1.69042 2.37184 2.03614 1.896C2.2381 1.61807 2.4828 1.37333 2.76074 1.17139C3.23665 0.825682 3.78386 0.680109 4.41407 0.611819C5.03168 0.544905 5.80591 0.546389 6.76563 0.546389H9.23438ZM6.76563 1.896C5.77586 1.896 5.0876 1.89738 4.55957 1.95459C4.0443 2.01043 3.76214 2.11349 3.55469 2.26416C3.39135 2.38284 3.24761 2.52662 3.12891 2.68994C2.97821 2.89736 2.8752 3.17967 2.81934 3.69483C2.76214 4.22279 2.76075 4.91131 2.76074 5.90088V10.0981C2.76074 11.0876 2.76221 11.7762 2.81934 12.3042C2.87516 12.8194 2.97829 13.1026 3.12891 13.3101C3.24754 13.4733 3.39147 13.6172 3.55469 13.7358C3.76213 13.8865 4.04438 13.9896 4.55957 14.0454C5.0876 14.1026 5.77586 14.103 6.76563 14.103H9.23438C10.2242 14.103 10.9124 14.1026 11.4404 14.0454C11.9556 13.9896 12.2379 13.8865 12.4453 13.7358C12.6086 13.6172 12.7525 13.4733 12.8711 13.3101C13.0217 13.1026 13.1248 12.8195 13.1807 12.3042C13.2378 11.7762 13.2393 11.0876 13.2393 10.0981V5.90088C13.2393 4.91131 13.2379 4.22279 13.1807 3.69483C13.1248 3.17969 13.0218 2.89736 12.8711 2.68994C12.7524 2.52667 12.6086 2.38281 12.4453 2.26416C12.2379 2.11355 11.9556 2.01041 11.4404 1.95459C10.9124 1.8974 10.2241 1.896 9.23438 1.896H6.76563Z" fill="currentColor" /> </svg>';
const DSH_ICON_API = '<svg width="14" height="14" viewBox="0 0 14 14" fill="none"> <path transform="translate(0.6689 1.073)" d="M11.4818 5.57813C11.4818 4.45301 11.4807 3.66237 11.4075 3.05908C11.3359 2.46953 11.2024 2.13852 10.9939 1.89441C10.9247 1.81341 10.8493 1.73801 10.7683 1.66882C10.5242 1.46033 10.1932 1.32686 9.60364 1.25525C9.00034 1.18198 8.20974 1.18091 7.0846 1.18091L5.57813 1.18091C4.45301 1.18091 3.66238 1.18198 3.05908 1.25525C2.46953 1.32686 2.13852 1.46033 1.89441 1.66882C1.81341 1.73801 1.73801 1.81341 1.66882 1.89441C1.46033 2.13852 1.32686 2.46953 1.25525 3.05908C1.18198 3.66238 1.18091 4.45301 1.18091 5.57813L1.18091 6.2771C1.18091 7.40218 1.18197 8.19288 1.25525 8.79614C1.32687 9.38553 1.46036 9.71674 1.66882 9.96082C1.73797 10.0417 1.81347 10.1173 1.89441 10.1864C2.13851 10.3948 2.46965 10.5275 3.05908 10.5991C3.66238 10.6724 4.45298 10.6735 5.57813 10.6735L7.0846 10.6735C8.20977 10.6735 9.00033 10.6724 9.60364 10.5991C10.1931 10.5275 10.5242 10.3948 10.7683 10.1864C10.8493 10.1173 10.9247 10.0417 10.9939 9.96082C11.2024 9.71674 11.3358 9.38553 11.4075 8.79614C11.4808 8.19288 11.4818 7.40218 11.4818 6.2771L11.4818 5.57813ZM12.6627 6.2771C12.6627 7.37222 12.6637 8.247 12.5798 8.93799C12.4942 9.64284 12.3133 10.2359 11.8928 10.7282C11.7834 10.8562 11.6637 10.9751 11.5356 11.0845C11.0434 11.5049 10.4511 11.6867 9.74634 11.7723C9.05525 11.8563 8.17999 11.8552 7.0846 11.8552L5.57813 11.8552C4.48273 11.8552 3.60747 11.8563 2.91638 11.7723C2.21157 11.6867 1.61933 11.5049 1.12708 11.0845C0.99901 10.9751 0.879281 10.8562 0.769898 10.7282C0.349454 10.2359 0.168506 9.64284 0.0828864 8.93799C-0.00101964 8.247 4.88512e-07 7.37222 6.47206e-07 6.2771L6.47206e-07 5.57813C6.47206e-07 4.48273 -0.00106163 3.60747 0.0828864 2.91638C0.168502 2.21168 0.349594 1.61928 0.769898 1.12708C0.879302 0.998981 0.998981 0.879302 1.12708 0.769898C1.61928 0.349594 2.21168 0.168502 2.91638 0.0828864C3.60747 -0.00106163 4.48273 6.47206e-07 5.57813 6.47206e-07L7.0846 6.47206e-07C8.17999 6.47206e-07 9.05525 -0.00106163 9.74634 0.0828864C10.451 0.168505 11.0434 0.349587 11.5356 0.769898C11.6637 0.879302 11.7834 0.998981 11.8928 1.12708C12.3131 1.61928 12.4942 2.21169 12.5798 2.91638C12.6638 3.60747 12.6627 4.48273 12.6627 5.57813L12.6627 6.2771Z" fill="currentColor"/> <path transform="translate(0.6689 1.073)" d="M6.02607 5.50955L6.44306 5.9274L3.84284 8.52762L3.425 8.11063L3.00715 7.69278L4.77253 5.9274L3.00715 4.16202L3.84284 3.32633L6.02607 5.50955Z" fill="currentColor"/> <path transform="translate(0.6689 1.073)" d="M9.23789 7.35397L9.23789 8.53488L6.96238 8.53488L6.96238 7.35397L9.23789 7.35397Z" fill="currentColor"/> </svg>';
const DSH_ICON_EDIT = '<svg width="14" height="14" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"> <path d="M9.94076 1.34942C10.7047 0.90231 11.6503 0.902415 12.4143 1.34942C12.7061 1.52015 12.9688 1.79118 13.3104 2.13284C13.6521 2.47448 13.9231 2.73721 14.0939 3.02894C14.5408 3.79294 14.5409 4.73856 14.0939 5.50251C13.9231 5.79415 13.652 6.05704 13.3104 6.39861L6.65932 13.0497C6.28068 13.4284 6.00695 13.7108 5.66543 13.9097C5.32391 14.1085 4.94315 14.2074 4.42705 14.3498L3.24394 14.6761C2.77527 14.8054 2.34538 14.9262 2.00131 14.9684C1.65196 15.0112 1.17964 15.0013 0.810764 14.6325C0.441921 14.2637 0.432107 13.7913 0.47486 13.442C0.517035 13.0979 0.6379 12.668 0.767181 12.1993L1.09352 11.0162C1.23588 10.5001 1.33481 10.1193 1.5336 9.77784C1.7325 9.43632 2.0149 9.1626 2.39355 8.78395L9.04466 2.13284C9.38625 1.79126 9.64911 1.52016 9.94076 1.34942ZM15.5427 14.8398H7.55223L8.96707 13.425H15.5427V14.8398ZM3.39382 9.78422C2.965 10.213 2.84244 10.3436 2.75709 10.49C2.67183 10.6366 2.61862 10.8079 2.45733 11.3925L2.13099 12.5756C2.00183 13.0439 1.92194 13.3419 1.88863 13.5536C2.10041 13.5204 2.39872 13.4416 2.86764 13.3123L4.05075 12.9859C4.63544 12.8246 4.80669 12.7715 4.95323 12.6862C5.09968 12.6008 5.23022 12.4783 5.65905 12.0494L10.721 6.98644L8.45577 4.72121L3.39382 9.78422ZM11.7 2.57079C11.3774 2.38198 10.9777 2.38198 10.6551 2.57079C10.5602 2.62647 10.4487 2.72931 10.0449 3.13311L9.45604 3.72094L11.7213 5.98617L12.3102 5.39833C12.7139 4.99457 12.8168 4.88307 12.8725 4.78818C13.0613 4.46561 13.0612 4.06585 12.8725 3.74326C12.8169 3.64827 12.7146 3.53752 12.3102 3.13311C11.9057 2.72863 11.795 2.6264 11.7 2.57079Z" fill="currentColor" /> </svg>';
const DSH_ICON_CODE = '<svg width="14" height="14" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"> <path fillRule="evenodd" clipRule="evenodd" d="M12.3368 1.53569L11.931 4.43172H14.8086V5.79673H11.7404L11.1962 9.67859H14.2839V11.0436H11.0056L10.4994 14.6529L9.14873 14.4643L9.62731 11.0436H5.75876L5.25252 14.6529L3.90186 14.4643L4.38043 11.0436H1.69141V9.67859H4.57104L5.11417 5.79673H2.21609V4.43172H5.30581L5.73724 1.34713L7.08995 1.53569L6.68414 4.43172H10.5527L10.9841 1.34713L12.3368 1.53569ZM5.94937 9.67859H9.81791L10.361 5.79673H6.49353L5.94937 9.67859Z" fill="currentColor" /> </svg>';
const DSH_ICON_SPARKLE = '<svg width="14" height="14" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"> <path d="M6.1 3.1Q6.6 7.8 11.3 8.3Q6.6 8.8 6.1 13.5Q5.6 8.8 0.9 8.3Q5.6 7.8 6.1 3.1Z" fill="currentColor" /> <path d="M11.9 1Q12.2 3.7 14.9 4Q12.2 4.3 11.9 7Q11.6 4.3 8.9 4Q11.6 3.7 11.9 1Z" fill="currentColor" /> <path d="M12.5 9.4Q12.7 11.4 14.7 11.6Q12.7 11.8 12.5 13.8Q12.3 11.8 10.3 11.6Q12.3 11.4 12.5 9.4Z" fill="currentColor" /> </svg>';
const DSH_ICON_THINK = '<svg width="14" height="14" viewBox="0 0 14 14" fill="none" xmlns="http://www.w3.org/2000/svg"> <path d="M7.06431 5.93342C7.68763 5.93342 8.19307 6.43904 8.19322 7.06233C8.19322 7.68573 7.68772 8.19123 7.06431 8.19123C6.44099 8.19113 5.9354 7.68567 5.9354 7.06233C5.93555 6.43911 6.44108 5.93353 7.06431 5.93342Z" fill="currentColor" /> <path fillRule="evenodd" clipRule="evenodd" d="M8.6815 0.963693C10.1169 0.447019 11.6266 0.374829 12.5633 1.31135C13.5 2.24805 13.4277 3.75776 12.911 5.19319C12.7126 5.74431 12.4386 6.31796 12.0965 6.89729C12.4969 7.54638 12.8141 8.19018 13.036 8.80647C13.5527 10.2419 13.6251 11.7516 12.6883 12.6883C11.7516 13.625 10.242 13.5527 8.8065 13.036C8.19022 12.8141 7.54641 12.4969 6.89732 12.0965C6.31797 12.4386 5.74435 12.7125 5.19322 12.911C3.75777 13.4276 2.2481 13.5 1.31138 12.5633C0.374859 11.6266 0.447049 10.1168 0.963724 8.68147C1.17185 8.10338 1.46321 7.50063 1.82896 6.8924C1.52182 6.35711 1.27235 5.82825 1.08872 5.31819C0.572068 3.88278 0.499714 2.37306 1.43638 1.43635C2.37308 0.499655 3.8828 0.572044 5.31822 1.08869C5.82828 1.27232 6.35715 1.5218 6.89243 1.82893C7.50066 1.46318 8.10341 1.17181 8.6815 0.963693ZM11.3573 8.01154C10.9083 8.62253 10.3901 9.22873 9.80943 9.8094C9.22877 10.3901 8.62255 10.9083 8.01158 11.3572C8.4257 11.5841 8.8287 11.7688 9.21275 11.9071C10.5456 12.3868 11.4246 12.2547 11.8397 11.8397C12.2548 11.4246 12.3869 10.5456 11.9071 9.21272C11.7688 8.82866 11.5841 8.42568 11.3573 8.01154ZM2.56529 8.02912C2.37344 8.39322 2.21495 8.74796 2.09263 9.08772C1.61291 10.4204 1.74512 11.2995 2.16001 11.7147C2.57505 12.1297 3.45415 12.2618 4.78697 11.7821C5.11057 11.6656 5.44786 11.5164 5.7938 11.3367C5.249 10.9223 4.70922 10.4533 4.19029 9.9344C3.57578 9.31987 3.03169 8.67633 2.56529 8.02912ZM6.90708 3.2469C6.24065 3.70479 5.5646 4.26321 4.91392 4.91389C4.26325 5.56456 3.70482 6.24063 3.24693 6.90705C3.72674 7.63325 4.32777 8.37459 5.03892 9.08576C5.64943 9.69627 6.28183 10.2265 6.90806 10.6678C7.59368 10.2025 8.2908 9.63076 8.96079 8.96076C9.6308 8.29075 10.2025 7.59366 10.6678 6.90803C10.2265 6.2818 9.69631 5.6494 9.08579 5.03889C8.37462 4.32773 7.63328 3.72672 6.90708 3.2469ZM11.7147 2.15998C11.2996 1.74509 10.4204 1.61288 9.08775 2.0926C8.74835 2.21479 8.39382 2.37271 8.03013 2.56428C8.67728 3.03065 9.31995 3.5758 9.93443 4.19026C10.4534 4.7092 10.9223 5.24896 11.3368 5.79377C11.5164 5.44785 11.6656 5.11052 11.7821 4.78694C12.2618 3.45416 12.1297 2.57502 11.7147 2.15998ZM4.91197 2.2176C3.57922 1.73788 2.70004 1.86995 2.28501 2.28498C1.87001 2.70003 1.73791 3.5792 2.21763 4.91194C2.31709 5.18822 2.44112 5.47427 2.58677 5.7674C3.01931 5.1887 3.51474 4.6158 4.06529 4.06526C4.61584 3.5147 5.18872 3.01928 5.76743 2.58674C5.47431 2.4411 5.18824 2.31706 4.91197 2.2176Z" fill="currentColor" /> </svg>';

const DSH_ICON_COPY = '<svg width="16" height="16" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"> <path d="M6.14929 4.02032C7.11197 4.02032 7.87983 4.02016 8.49597 4.07598C9.12128 4.13269 9.65792 4.25188 10.1415 4.53106C10.7202 4.8653 11.2008 5.3459 11.535 5.92462C11.8142 6.40818 11.9334 6.94481 11.9901 7.57012C12.0459 8.18625 12.0458 8.95419 12.0458 9.9168C12.0458 10.8795 12.0459 11.6473 11.9901 12.2635C11.9334 12.8888 11.8142 13.4254 11.535 13.909C11.2008 14.4877 10.7202 14.9683 10.1415 15.3025C9.65792 15.5817 9.12128 15.7009 8.49597 15.7576C7.87984 15.8134 7.11196 15.8133 6.14929 15.8133C5.18667 15.8133 4.41874 15.8134 3.80261 15.7576C3.1773 15.7009 2.64067 15.5817 2.1571 15.3025C1.5784 14.9683 1.09778 14.4877 0.76355 13.909C0.484366 13.4254 0.365184 12.8888 0.308472 12.2635C0.252649 11.6473 0.252808 10.8795 0.252808 9.9168C0.252808 8.95418 0.252664 8.18625 0.308472 7.57012C0.365184 6.94481 0.484366 6.40818 0.76355 5.92462C1.09777 5.34589 1.57839 4.86529 2.1571 4.53106C2.64067 4.25188 3.1773 4.13269 3.80261 4.07598C4.41874 4.02017 5.18666 4.02032 6.14929 4.02032ZM6.14929 5.37774C5.16181 5.37774 4.46634 5.37761 3.92566 5.42657C3.39434 5.47472 3.07859 5.56574 2.83582 5.70587C2.4632 5.92106 2.15354 6.2307 1.93835 6.60333C1.79823 6.8461 1.70721 7.16185 1.65906 7.69317C1.6101 8.23385 1.61023 8.92933 1.61023 9.9168C1.61023 10.9043 1.61009 11.5998 1.65906 12.1404C1.70721 12.6717 1.79823 12.9875 1.93835 13.2303C2.15356 13.6029 2.46321 13.9126 2.83582 14.1277C3.07859 14.2679 3.39434 14.3589 3.92566 14.407C4.46634 14.456 5.16182 14.4559 6.14929 14.4559C7.13682 14.4559 7.83224 14.456 8.37292 14.407C8.90425 14.3589 9.21999 14.2679 9.46277 14.1277C9.83535 13.9126 10.145 13.6029 10.3602 13.2303C10.5004 12.9875 10.5914 12.6717 10.6395 12.1404C10.6885 11.5998 10.6884 10.9043 10.6884 9.9168C10.6884 8.92934 10.6885 8.23384 10.6395 7.69317C10.5914 7.16185 10.5004 6.8461 10.3602 6.60333C10.1451 6.23071 9.83536 5.92107 9.46277 5.70587C9.21999 5.56574 8.90424 5.47472 8.37292 5.42657C7.83224 5.3776 7.13682 5.37774 6.14929 5.37774ZM9.80164 0.367975C10.7638 0.367975 11.5314 0.36788 12.1473 0.423639C12.7726 0.480307 13.3093 0.598759 13.7928 0.877741C14.3717 1.21192 14.8521 1.69355 15.1864 2.27227C15.4655 2.75574 15.5857 3.29164 15.6425 3.9168C15.6983 4.53301 15.6971 5.3016 15.6971 6.26446V7.82989C15.6971 8.29264 15.6989 8.58993 15.6649 8.84844C15.4668 10.3525 14.401 11.5738 12.9833 11.9988V10.5467C13.6973 10.1903 14.2105 9.49662 14.3192 8.67169C14.3387 8.52347 14.3407 8.3358 14.3407 7.82989V6.26446C14.3407 5.27706 14.3398 4.58149 14.2909 4.04083C14.2428 3.50968 14.1526 3.19372 14.0126 2.95098C13.7974 2.57849 13.4876 2.26869 13.1151 2.05352C12.8724 1.91347 12.5564 1.82237 12.0253 1.77423C11.4847 1.72528 10.7888 1.7254 9.80164 1.7254H7.71472C6.7562 1.72558 5.92665 2.27697 5.52332 3.07891H4.07019C4.54221 1.51132 5.9932 0.368186 7.71472 0.367975H9.80164Z" fill="currentColor" /> </svg>';
const DSH_ICON_LIKE = '<svg width="16" height="16" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"> <path d="M8.27868 0.811572C8.81991 0.142194 9.79022 0.0421835 10.4538 0.557601L10.5823 0.669306L10.6066 0.693544L10.6097 0.695652L10.6392 0.725159C11.355 1.44679 11.6337 2.49468 11.3716 3.47669L11.3706 3.48091L11.3611 3.51674L11.3601 3.51885L10.889 5.22604C10.8796 5.25997 10.8707 5.29157 10.8627 5.32088C10.8934 5.32095 10.927 5.32194 10.9628 5.32194H11.9007C12.4264 5.32194 12.7831 5.319 13.0651 5.36725C14.8182 5.66719 15.9851 7.34568 15.6565 9.09357C15.6036 9.37487 15.477 9.7092 15.294 10.2022L14.3371 12.7798C14.1402 13.3104 13.9774 13.7518 13.8102 14.1024C13.6376 14.4645 13.4386 14.7793 13.1442 15.0424C12.9712 15.197 12.7802 15.3303 12.5751 15.4386C12.226 15.6231 11.8608 15.7 11.4612 15.7358C11.0743 15.7705 10.6035 15.7695 10.0375 15.7695H4.87377C4.08053 15.7695 3.42928 15.7702 2.90734 15.7137C2.37212 15.6557 1.88991 15.5311 1.46676 15.2237C1.22415 15.0474 1.01078 14.8339 0.834466 14.5914C0.527021 14.1682 0.401373 13.686 0.343384 13.1508C0.286822 12.6287 0.287531 11.9769 0.287531 11.1833V9.51405C0.287531 8.84778 0.281347 8.36714 0.399237 7.9565C0.671152 7.00935 1.41115 6.26832 2.35829 5.99638C2.76894 5.87849 3.24958 5.88573 3.91585 5.88573C4.11983 5.88573 4.14548 5.88319 4.16244 5.88046C4.23532 5.86863 4.30409 5.83663 4.35845 5.78667C4.3711 5.77504 4.38761 5.75604 4.51442 5.59488L8.25655 0.838972L8.2576 0.837918L8.27868 0.811572ZM1.69122 11.1833C1.69122 12.0082 1.69217 12.5711 1.73865 13.0001C1.78371 13.4157 1.86473 13.6221 1.96943 13.7662C2.0592 13.8898 2.16733 13.9989 2.29085 14.0887C2.43501 14.1934 2.64216 14.2744 3.05803 14.3195C3.45897 14.3629 3.97637 14.3656 4.7157 14.3659C4.30801 13.8053 4.06453 13.1171 4.06444 12.371V8.59406H5.46813V12.371C5.46838 13.4733 6.36166 14.3669 7.46407 14.3669H10.0375C10.6286 14.3669 11.0269 14.3663 11.3369 14.3385C11.6339 14.3118 11.7956 14.2638 11.9196 14.1983C12.0241 14.1431 12.1213 14.0747 12.2094 13.996C12.314 13.9025 12.4151 13.7678 12.5435 13.4986C12.6774 13.2176 12.8162 12.845 13.0219 12.2909L13.9788 9.71322C14.1848 9.15816 14.2531 8.96731 14.2781 8.83433C14.4618 7.85692 13.8093 6.91895 12.8291 6.75092C12.6957 6.7281 12.4928 6.72458 11.9007 6.72458H10.9628C10.7737 6.72458 10.5693 6.72657 10.4 6.70666C10.2211 6.68562 9.96702 6.63024 9.74771 6.43161C9.64454 6.33811 9.55957 6.2261 9.4969 6.10177C9.3639 5.83784 9.37799 5.57899 9.40521 5.40097C9.431 5.23261 9.48672 5.03616 9.53694 4.85404L10.008 3.14579L10.0175 3.11102C10.1488 2.61338 10.0078 2.08338 9.64654 1.71681L9.6086 1.67887L9.55064 1.64304C9.48795 1.62043 9.41425 1.63814 9.36938 1.69362L9.35779 1.70627L9.35884 1.70732L5.61672 6.46217C5.51822 6.58735 5.42237 6.7133 5.30689 6.81942C5.05075 7.05471 4.73126 7.20939 4.38796 7.26519C4.23315 7.29032 4.07513 7.28837 3.91585 7.28837C3.15356 7.28837 2.91916 7.2957 2.7461 7.34528C2.26364 7.48379 1.88564 7.86081 1.74708 8.34325C1.69738 8.51636 1.69122 8.7511 1.69122 9.51405V11.1833Z" fill="currentColor" /> </svg>';
const DSH_ICON_DISLIKE = '<svg width="16" height="16" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"> <path d="M7.72451 15.1086C7.18929 15.7705 6.22975 15.8694 5.57357 15.3597L5.44643 15.2492L5.42247 15.2253L5.41934 15.2232L5.39016 15.194C4.68239 14.4804 4.40679 13.4441 4.66589 12.473L4.66693 12.4689L4.67631 12.4334L4.67735 12.4314L5.14318 10.7431C5.15243 10.7096 5.1613 10.6783 5.16923 10.6493C5.13878 10.6493 5.10558 10.6483 5.07023 10.6483H4.14274C3.62288 10.6483 3.27015 10.6512 2.9912 10.6035C1.25757 10.3069 0.103662 8.64702 0.42863 6.91854C0.480965 6.64037 0.606164 6.30975 0.787119 5.82223L1.73336 3.27321C1.92812 2.74852 2.08912 2.31209 2.25442 1.96535C2.42515 1.60724 2.62191 1.29594 2.91304 1.03578C3.08408 0.882951 3.273 0.751121 3.47579 0.643944C3.82102 0.461504 4.18214 0.38551 4.57731 0.350066C4.95993 0.315784 5.42553 0.316718 5.98521 0.316718H11.0916C11.876 0.316718 12.52 0.31607 13.0362 0.37195C13.5655 0.429293 14.0423 0.552534 14.4608 0.856536C14.7007 1.03085 14.9117 1.24193 15.086 1.48181C15.3901 1.90027 15.5143 2.37709 15.5717 2.90638C15.6276 3.42269 15.6269 4.06721 15.6269 4.85202V6.50274C15.6269 7.1616 15.633 7.6369 15.5164 8.04299C15.2475 8.97962 14.5158 9.71242 13.5791 9.98133C13.173 10.0979 12.6977 10.0908 12.0389 10.0908C11.8372 10.0908 11.8118 10.0933 11.795 10.096C11.723 10.1077 11.6549 10.1393 11.6012 10.1887C11.5887 10.2002 11.5724 10.219 11.447 10.3784L7.74639 15.0815L7.74535 15.0825L7.72451 15.1086ZM14.2388 4.85202C14.2388 4.03628 14.2379 3.47965 14.1919 3.05541C14.1473 2.64443 14.0672 2.4403 13.9637 2.29779C13.8749 2.17562 13.768 2.06769 13.6458 1.9789C13.5033 1.87532 13.2984 1.79523 12.8872 1.75067C12.4907 1.70773 11.979 1.70511 11.2479 1.70482C11.6511 2.25917 11.8918 2.93968 11.8919 3.67755V7.41251H10.5038V3.67755C10.5036 2.58745 9.62023 1.70378 8.53007 1.70378H5.98521C5.40065 1.70378 5.00679 1.70442 4.70028 1.73192C4.40651 1.7583 4.24662 1.80571 4.12399 1.87052C4.02069 1.92511 3.92452 1.99276 3.8374 2.07061C3.73401 2.16306 3.634 2.2962 3.50705 2.56249C3.37462 2.84027 3.23734 3.20873 3.03393 3.75675L2.08768 6.30578C1.88395 6.85467 1.81646 7.0434 1.79172 7.1749C1.61005 8.14146 2.25533 9.06902 3.22464 9.23517C3.35654 9.25774 3.55717 9.26123 4.14274 9.26123H5.07023C5.25717 9.26123 5.4593 9.25926 5.62672 9.27894C5.80364 9.29975 6.05492 9.35452 6.27179 9.55094C6.37381 9.6434 6.45784 9.75417 6.51982 9.87712C6.65133 10.1381 6.6374 10.3941 6.61048 10.5701C6.58498 10.7366 6.52988 10.9309 6.48022 11.111L6.01439 12.8003L6.00501 12.8347C5.87513 13.3268 6.01464 13.8509 6.37184 14.2134L6.40935 14.2509L6.46667 14.2863C6.52866 14.3087 6.60155 14.2912 6.64591 14.2363L6.65738 14.2238L6.65633 14.2228L10.3569 9.52072C10.4543 9.39693 10.5491 9.27238 10.6633 9.16744C10.9166 8.93476 11.2325 8.7818 11.572 8.72662C11.7251 8.70177 11.8814 8.70369 12.0389 8.70369C12.7927 8.70369 13.0245 8.69645 13.1956 8.64742C13.6727 8.51045 14.0465 8.13761 14.1836 7.66053C14.2327 7.48935 14.2388 7.25721 14.2388 6.50274V4.85202Z" fill="currentColor" /> </svg>';


const DSH_ICON_CHEVRON_RIGHT = '<svg width="14" height="14" viewBox="0 0 14 14" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M5.5 2.15137L5.92383 2.57617L8.65137 5.30273C8.90706 5.55843 9.13382 5.78438 9.29785 5.98828C9.46883 6.20088 9.61756 6.44405 9.66602 6.75C9.69222 6.91565 9.69222 7.08435 9.66602 7.25C9.61756 7.55595 9.46883 7.79912 9.29785 8.01172C9.13382 8.21561 8.90706 8.44157 8.65137 8.69727L5.92383 11.4238L5.5 11.8486L4.65137 11L5.07617 10.57617L7.80273 7.84863C8.07732 7.57405 8.24848 7.40124 8.3623 7.25977C8.46904 7.12709 8.47813 7.07728 8.48047 7.0625C8.48703 7.02105 8.48703 6.97895 8.48047 6.9375C8.47813 6.92272 8.46904 6.87291 8.3623 6.74023C8.24848 6.59876 8.07732 6.42595 7.80273 6.15137L5.07617 3.42383L4.65137 3L5.5 2.15137Z" fill="currentColor"/></svg>';
const DSH_ICON_CHEVRON_DOWN = '<svg width="14" height="14" viewBox="0 0 14 14" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M11.8486 5.5L11.4238 5.92383L8.69727 8.65137C8.44157 8.90706 8.21562 9.13382 8.01172 9.29785C7.79912 9.46883 7.55595 9.61756 7.25 9.66602C7.08435 9.69222 6.91565 9.69222 6.75 9.66602C6.44405 9.61756 6.20088 9.46883 5.98828 9.29785C5.78438 9.13382 5.55843 8.90706 5.30273 8.65137L2.57617 5.92383L2.15137 5.5L3 4.65137L3.42383 5.07617L6.15137 7.80273C6.42595 8.07732 6.59876 8.24849 6.74023 8.3623C6.87291 8.46904 6.92272 8.47813 6.9375 8.48047C6.97895 8.48703 7.02105 8.48703 7.0625 8.48047C7.07728 8.47813 7.12709 8.46904 7.25977 8.3623C7.40124 8.24849 7.57405 8.07732 7.84863 7.80273L10.5762 5.07617L11 4.65137L11.8486 5.5Z" fill="currentColor"/></svg>';

// Theme-toggle glyphs (dsh ui-primitives ic_ds_light_outline_16 /
// ic_ds_dark_outline_16): monochrome currentColor, so the button renders white
// on dark and black on light (the theme seat shows the target theme's icon).
const DSH_ICON_LIGHT = '<svg width="16" height="16" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"> <path d="M11.3496 8C11.3496 6.14985 9.85015 4.65039 8 4.65039C6.14985 4.65039 4.65039 6.14985 4.65039 8C4.65039 9.85015 6.14985 11.3496 8 11.3496C9.85015 11.3496 11.3496 9.85015 11.3496 8ZM12.6504 8C12.6504 10.5681 10.5681 12.6504 8 12.6504C5.43188 12.6504 3.34961 10.5681 3.34961 8C3.34961 5.43188 5.43188 3.34961 8 3.34961C10.5681 3.34961 12.6504 5.43188 12.6504 8Z" fill="currentColor" /> <path d="M8.65039 0.5V2.5H7.34961V0.5H8.65039Z" fill="currentColor" /> <path d="M8.65039 13.5V15.5H7.34961V13.5H8.65039Z" fill="currentColor" /> <path d="M3.15808 2.24035L4.57229 3.65456L3.6525 4.57435L2.23829 3.16014L3.15808 2.24035Z" fill="currentColor" /> <path d="M12.3505 11.4327L13.7647 12.8469L12.8449 13.7667L11.4307 12.3525L12.3505 11.4327Z" fill="currentColor" /> <path d="M2.24537 12.8469L3.65958 11.4327L4.57937 12.3525L3.16516 13.7667L2.24537 12.8469Z" fill="currentColor" /> <path d="M11.4377 3.65455L12.852 2.24033L13.7718 3.16012L12.3575 4.57434L11.4377 3.65455Z" fill="currentColor" /> <path d="M0.5 7.35461H2.5V8.6554H0.5L0.5 7.35461Z" fill="currentColor" /> <path d="M13.5 7.35461H15.5V8.6554H13.5V7.35461Z" fill="currentColor" /> </svg>';
const DSH_ICON_DARK = '<svg width="16" height="16" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"> <path d="M13.2764 9.52324C12.5607 9.97754 11.7177 10.242 10.7812 10.242C8.11386 10.2419 5.95042 8.07997 5.9502 5.41289C5.9502 4.48128 6.21453 3.61071 6.67188 2.87285C4.30332 3.4658 2.54992 5.60845 2.5498 8.16093C2.5498 11.1712 4.99103 13.6102 8 13.6102C10.5383 13.6102 12.6709 11.8724 13.2764 9.52324ZM7.05078 5.41289C7.051 7.47224 8.72116 9.1423 10.7812 9.14238C11.9248 9.14238 12.887 8.63397 13.5781 7.8084C13.7266 7.63106 13.9701 7.56547 14.1875 7.64433C14.4049 7.72329 14.5497 7.9297 14.5498 8.16093C14.5498 11.7766 11.6161 14.7098 8 14.7098C4.38402 14.7098 1.4502 11.7792 1.4502 8.16093C1.45033 4.54322 4.3812 1.61015 8 1.61015C8.23027 1.61015 8.43585 1.75352 8.51562 1.96953C8.59536 2.18554 8.53241 2.42829 8.35742 2.57793C7.55573 3.26311 7.05078 4.27876 7.05078 5.41289Z" fill="currentColor" /> </svg>';

// Tool classification (dsh tool-call-model TOOL_VARIANTS): known tool name ->
// row variant; unknown tools land on the generic "Tool call" row. The dsh
// tool names are canonical; the shutu legacy names (pre-alignment) stay mapped
// so old sessions still classify the same rows.
const TOOL_VARIANTS = {
  bash: "bash", pwsh: "bash", run_command: "bash",
  terminal_start: "bash", terminal_write: "bash", terminal_read: "bash",
  terminal_signal: "bash", terminal_stop: "bash",
  read: "read", web_fetch: "read", read_file: "read", fs_read: "read",
  web_search: "search", grep: "search", glob: "search", fs_search: "search",
  write: "write", fs_write: "write", edit: "edit",
  run_code: "code", code_run: "code",
  list: "others", fs_list: "others",
};

// Tool-owned titles that refine a generic row variant without replacing it
// (dsh TOOL_TITLES): pwsh keeps its bash-row family with its own title.
const TOOL_TITLES = { pwsh: "Pwsh" };
// Figma row titles per variant (dsh design literals, not translatable copy).
const VARIANT_TITLES = { search: "Search", read: "Read", bash: "Bash", write: "Write", edit: "Edit", code: "Code", others: "Tool call" };
const VARIANT_ICONS = {
  search: DSH_ICON_SEARCH, read: DSH_ICON_BROWSE, bash: DSH_ICON_API,
  write: DSH_ICON_EDIT, edit: DSH_ICON_EDIT, code: DSH_ICON_CODE, others: DSH_ICON_SPARKLE,
};
// The persistent-shell row carries the shell's display name (dsh pwsh row);
// refined from the settings terminal_shell row when settings load.
const SHELL_TITLES = { powershell: "Pwsh", gitbash: "Git Bash", wsl: "WSL", cmd: "Cmd", off: "Terminal" };
let termShellTitle = "Pwsh";

function toolVariant(name) { return TOOL_VARIANTS[name] || "others"; }
function toolRowTitle(name, variant) {
  // dsh: tool-owned title first (pwsh → "Pwsh"); shutu's persistent terminal
  // rows carry the configured shell's display name (Pwsh / Git Bash / WSL).
  if (variant === "bash" && name && name.startsWith("terminal_")) return termShellTitle;
  if (TOOL_TITLES[name]) return TOOL_TITLES[name];
  return VARIANT_TITLES[variant] || "Tool call";
}
function firstLine(text) { const i = text.indexOf("\n"); return i === -1 ? text : text.slice(0, i); }
function latestLine(text) { const v = text.trimEnd(); const i = v.lastIndexOf("\n"); return i === -1 ? v : v.slice(i + 1); }
function renderHelpBody(text) {
  return String(text || "").split("\n").map((line) => {
    const match = line.match(/^(\s*)(\/\S+)(\s+)(.*)$/);
    if (!match) return `<div class="command-help-line">${esc(line)}</div>`;
    return `<div class="command-help-line">${esc(match[1])}<strong>${esc(match[2])}</strong>${esc(match[3] + match[4])}</div>`;
  }).join("");
}
function slashCommandName(text) {
  const match = String(text || "").trim().match(/^\/([^\s]+)/);
  return match ? match[1].toLowerCase() : "";
}
function isBuiltInSlashCommand(name) {
  const item = webCommands.find((entry) => entry.name === name);
  // A catalogued skill is a normal model invocation, not a host command card.
  // Unknown slash names still go through the host command dispatcher and use
  // the generic dsh command row with the server's error text.
  return !item || item.kind !== "skill";
}
function queueSlashCommand(ev) {
  const name = slashCommandName(ev && ev.summary);
  if (!name || !isBuiltInSlashCommand(name)) return false;
  pendingSlashCommands.push({ name, seq: ev.seq });
  return true;
}
function takeSlashCommand() { return pendingSlashCommands.shift() || null; }
function parseToolArgs(raw) {
  try { const v = JSON.parse(raw || "{}"); return v && typeof v === "object" ? v : {}; } catch { return {}; }
}
function prettyToolArgs(raw) {
  try { return JSON.stringify(JSON.parse(raw || "{}"), null, 2); } catch { return raw || ""; }
}
// Summary key preference per variant (dsh SUMMARY_KEYS): the collapsed row
// shows the meaningful arg (command / path / query / description). The search
// variant joins the queries array (dsh), falling back to query/pattern.
function toolSummary(name, variant, raw) {
  const args = parseToolArgs(raw);
  const pick = (keys) => { for (const k of keys) { const v = args[k]; if (typeof v === "string" && v !== "") return firstLine(v); } return ""; };
  let s = "";
  if (variant === "bash") s = pick(["description", "command", "text"]);
  else if (variant === "read") s = pick(["path", "file_path", "url"]);
  else if (variant === "search") {
    if (Array.isArray(args.queries)) {
      const qs = args.queries.filter((q) => typeof q === "string" && q !== "");
      if (qs.length > 0) s = qs.map(firstLine).join(", ");
    }
    if (s === "") s = pick(["query", "pattern", "url"]);
  }
  else if (variant === "write" || variant === "edit") s = pick(["path", "file_path"]);
  else if (variant === "code") s = pick(["description"]);
  if (s === "") {
    for (const v of Object.values(args)) { if (typeof v === "string" && v !== "") { s = firstLine(v); break; } }
  }
  return s === "" ? (name || "Tool call") : s;
}

// addReasoning renders (or appends to) the in-place Think disclosure row
// (dsh ReasoningRow): while reasoning streams, the collapsed summary follows
// the LATEST line with the running sweep; once the step settles it shows the
// FIRST line. The body is the full reasoning text. evSeq detects the step
// boundary: a delta whose seq jumps past an assistant/message (and its tool
// events) starts a FRESH Think row, exactly one per assistant step (dsh).
function addReasoning(reasoningText, timeIso, evSeq) {
  const inner = msgInner();
  const newStep = currentReasoningNode === null
    || (evSeq != null && lastReasoningSeq !== 0 && evSeq > lastReasoningSeq + 1);
  if (newStep) currentReasoningNode = null;
  let node = currentReasoningNode;
  if (!node) {
    node = document.createElement("div");
    node.className = "msg reasoning";
    node.dataset.state = "running";
    node.innerHTML = `
      <div class="dsh-row" role="button" tabindex="0">
        <span class="dsh-leading">${DSH_ICON_THINK}</span>
        <span class="dsh-title">Think</span>
        <span class="dsh-sep" aria-hidden></span>
        <span class="dsh-summary" data-follow-end="1"></span>
        <span class="dsh-caret">▸</span>
      </div>
      <div class="dsh-think-body"></div>`;
    const row = node.querySelector(".dsh-row");
    const toggle = () => { node.classList.toggle("open"); scrollToBottom(); };
    row.addEventListener("click", toggle);
    row.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(); }
    });
    inner.appendChild(node);
    currentReasoningNode = node;
  }
  if (evSeq != null) lastReasoningSeq = evSeq;
  const body = node.querySelector(".dsh-think-body");
  body.textContent = (body.textContent || "") + reasoningText;
  node.querySelector(".dsh-summary").textContent = latestLine(body.textContent);
  node.dataset.state = "running";
  scrollToBottom(true);
}

// settleReasoning flips every open Think row to its settled look: the
// collapsed summary becomes the reasoning's first line and the sweep stops.
// It settles ALL running rows so a cancelled or failed step can never leave a
// row sweeping forever.
function settleReasoning() {
  const inner = msgInner();
  const rows = inner.querySelectorAll(":scope > .msg.reasoning[data-state='running']");
  for (const node of rows) {
    node.dataset.state = "ok";
    const body = node.querySelector(".dsh-think-body");
    const summary = node.querySelector(".dsh-summary");
    if (summary) {
      summary.textContent = body ? firstLine(body.textContent) : "";
      summary.removeAttribute("data-follow-end");
    }
  }
  if (currentReasoningNode && currentReasoningNode.dataset.state === "running") {
    currentReasoningNode.dataset.state = "ok";
  }
  currentReasoningNode = null;
}

// renderToolRow draws one dsh tool row (figma 122:9479): 24px single line —
// [16 leading icon] title · 2x2 separator · FILL-truncated summary — with the
// state semantics (running sweep / ok icon / error red dot + failure line).
// tool/start creates the running row; tool/result and tool/error settle the
// same row in place (paired by call_id). Expanding shows the IN/OUT card.
function renderToolRow(ev) {
  const inner = msgInner();
  const name = ev.tool_name || "";
  const variant = toolVariant(name);
  const isStart = ev.type === "tool/start";
  const isErr = ev.type === "tool/error";
  const state = isStart ? "running" : (isErr ? "error" : "ok");
  const callID = ev.call_id || "";
  let node = null;
  if (callID) {
    // Reuse the row of the same call whenever one exists: a reconnect replay
    // may deliver tool/start again, and result/error must settle the exact
    // running row — never leave an orphaned duplicate sweeping (yellow dot).
    node = inner.querySelector(`.msg.tool[data-call="${String(callID).replace(/"/g, "\\\"")}"]`);
  }
  if (!node) {
    node = document.createElement("div");
    node.className = "msg tool";
    if (callID) node.dataset.call = String(callID);
    inner.appendChild(node);
  }
  const seq = ev.seq == null ? "" : String(ev.seq);
  node.dataset.state = state;
  node.dataset.seq = seq;
  const args = prettyToolArgs(ev.tool_args);
  // tool/error carries the actionable tool error in tool_output. summary is
  // the compact event label (for example, "tool grep error → .") and is only
  // a compatibility fallback for older event payloads.
  const output = isErr ? (ev.tool_output || ev.summary || "") : (ev.tool_output || "");
  const failureLine = isErr ? firstLine(ev.tool_output || ev.summary || "") : "";
  const summary = failureLine || toolSummary(name, variant, ev.tool_args);
  // dsh: bash/pwsh rows expand into a TERMINAL card; read rows into a
  // line-numbered READ card; grep into a grouped SEARCH card; code rows show
  // the program as the IN body; everything else keeps the IN/OUT card.
  const isBash = variant === "bash";
  const isFileRead = variant === "read" && (name === "read" || name === "read_file" || name === "fs_read");
  const isGrep = variant === "search" && (name === "grep" || name === "fs_search");
  const isWebSearch = name === "web_search";
  const expandable = isBash || isFileRead || isGrep || isWebSearch || !!(args || output);
  const wasOpen = node.classList.contains("open");
  toolMeta[seq] = { name: name || "Tool call", args: ev.tool_args || "", output: output, error: isErr };
  const bodyHtml = isBash
    ? `<div class="dsh-term">
         <div class="dsh-term-prompt">
           <span class="dsh-term-dot" aria-hidden></span>
           <span class="dsh-term-cmd">${esc(summary)}</span>
         </div>
         ${output ? `<div class="dsh-term-out">${esc(output)}</div>` : `<div class="dsh-term-empty">（无输出）</div>`}
       </div>`
    : isFileRead && output
      ? readCardHtml(output, summary)
      : isGrep && output
        ? (searchCardHtml(output) || `<div class="dsh-io-card">${ioCardSections(args, output, isErr)}</div>`)
        : isWebSearch && output
          ? (webCardHtml(output) || `<div class="dsh-io-card">${ioCardSections(args, output, isErr)}</div>`)
          : `<div class="dsh-io-card">${ioCardSections(args, output, isErr, variant)}</div>`;
  // dsh fileLink: single-file tools (read/write/edit) show the path as an
  // underlined link-style summary.
  const singleFile = !failureLine && (variant === "read" || variant === "write" || variant === "edit");
  node.innerHTML = `
    <div class="dsh-row" role="button" tabindex="0">
      <span class="dsh-leading">${isErr ? '<span class="dsh-statedot dsh-statedot-err"></span>' : VARIANT_ICONS[variant]}</span>
      <span class="dsh-title">${esc(toolRowTitle(name, variant))}</span>
      <span class="dsh-sep" aria-hidden></span>
      <span class="dsh-summary${failureLine ? " dsh-summary-err" : ""}${singleFile ? " dsh-filelink" : ""}">${esc(summary)}</span>
      <span class="dsh-caret">▸</span>
    </div>
    ${expandable ? bodyHtml : ""}`;
  const row = node.querySelector(".dsh-row");
  const toggle = () => {
    if (!expandable) { openDetails(seq); return; }
    node.classList.toggle("open");
    openDetails(seq);
    scrollToBottom();
  };
  row.addEventListener("click", toggle);
  row.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(); }
  });
  // Card chrome: the read card's copy control and its head/tail expand toggle.
  node.querySelectorAll("[data-read-copy]").forEach((btn) => {
    btn.addEventListener("click", (e) => {
      e.stopPropagation();
      navigator.clipboard?.writeText(btn.dataset.readCopy || "").catch(() => {});
      const t = btn.textContent; btn.textContent = "复制成功";
      setTimeout(() => { btn.textContent = t; }, 1000);
    });
  });
  node.querySelectorAll(".dsh-read-expand").forEach((btn) => {
    btn.addEventListener("click", (e) => {
      e.stopPropagation();
      btn.closest(".dsh-read").classList.toggle("expanded");
    });
  });
  if (wasOpen && expandable) node.classList.add("open");
  scrollToBottom(true);
}

// ioCardSections renders the IN/OUT card sections (dsh ioCard 1249:35657). The
// code variant shows the PROGRAM as the IN body (dsh deriveBody), not the JSON
// envelope.
function ioCardSections(args, output, isErr, variant) {
  let inText = args;
  if (variant === "code" && args) {
    try {
      const v = JSON.parse(args);
      if (typeof v.code === "string" && v.code !== "") inText = v.code;
    } catch (_) { /* fall back to the raw args */ }
  }
  return `
    ${inText ? `<div class="dsh-io-sec"><span class="dsh-io-label">IN</span><span class="dsh-io-text">${esc(inText)}</span></div>` : ""}
    ${inText && output ? `<span class="dsh-io-divider" aria-hidden></span>` : ""}
    ${output ? `<div class="dsh-io-sec"><span class="dsh-io-label">OUT</span><span class="dsh-io-text${isErr ? " dsh-io-err" : ""}">${esc(output)}</span></div>` : ""}`;
}

// readCardHtml renders a dsh ReadBlock: banner (path + "显示 N / M 行" note +
// copy) over line-numbered rows. All lines are in the DOM; the head/tail cap
// (16 lines) and the expand toggle are pure CSS, so no slicing arithmetic can
// drift from the rendering.
function readCardHtml(text, label) {
  const lines = text.split("\n");
  const capped = lines.length > 16;
  const row = (l, i) => `<div class="dsh-read-line"><span class="dsh-read-gutter" aria-hidden>${i + 1}</span><span class="dsh-read-text">${esc(l)}</span></div>`;
  return `<div class="dsh-read"${capped ? "" : ' data-full="1"'}>
    <div class="dsh-read-banner">
      <span class="dsh-read-label">${esc(label)}</span>
      <span class="dsh-read-count">${capped ? "显示 16 / " + lines.length + " 行" : lines.length + " 行"}</span>
      <button type="button" class="dsh-read-copy" data-read-copy="${esc(text)}">复制</button>
    </div>
    <div class="dsh-read-body">
      ${lines.map((l, i) => row(l, i)).join("")}
    </div>
    ${capped ? `<div class="dsh-read-mid"><button type="button" class="dsh-read-expand">… 其余 ${lines.length - 16} 行</button></div>` : ""}
  </div>`;
}

// webCardHtml renders a dsh WebBlock-style card for web_search: the answer
// content on top, then one citation row per "- [label](url) — meta" source
// line (the URL underlined like dsh web links). Non-citation text (e.g. the
// trailing "Cite the relevant URLs..." note) is dropped; returns null when
// nothing parses, so the caller falls back to the plain OUT card.
function webCardHtml(text) {
  const citeRe = /^- \[([^\]]*)\]\(([^)]*)\)(.*)$/;
  const content = [];
  const cites = [];
  let inSources = false;
  for (const line of text.split("\n")) {
    if (line === "Sources:") { inSources = true; continue; }
    const m = citeRe.exec(line);
    if (m) {
      cites.push({ label: m[1], url: m[2], meta: m[3].replace(/^\s*\u2014\s*/, "") });
      continue;
    }
    if (inSources) continue;
    if (line.trim() !== "") content.push(line);
  }
  if (cites.length === 0) return null;
  let html = `<div class="dsh-web">`;
  if (content.length > 0) {
    html += `<div class="dsh-web-content">${esc(content.join("\n"))}</div>`;
  }
  html += `<div class="dsh-web-cites">`;
  for (const c of cites) {
    html += `<div class="dsh-web-cite">
      <span class="dsh-web-label">${esc(c.label)}</span>
      <span class="dsh-web-url">${esc(c.url)}</span>
      ${c.meta ? `<span class="dsh-web-meta">${esc(c.meta)}</span>` : ""}
    </div>`;
  }
  html += `</div></div>`;
  return html;
}

// searchCardHtml renders a dsh SearchBlock: one file header per matched file
// with its indented match lines. Parses the dsh grep result format — per-file
// "path\nLine N: text" sections with a "Found N matches" header and the
// could-not-save footer — and drops lines that are not match rows (header,
// footer, blank separators).
function searchCardHtml(text) {
  const lineRe = /^Line (\d+): (.*)$/;
  const groups = new Map();
  const order = [];
  let current = null;
  for (const line of text.split("\n")) {
    const m = lineRe.exec(line);
    if (m) {
      if (current === null) { current = "?"; groups.set(current, []); order.push(current); }
      groups.get(current).push({ n: m[1], t: m[2] });
      continue;
    }
    if (line === "" || line.startsWith("Found ") || line.startsWith("(") || line === "No matches found") continue;
    current = line;
    if (!groups.has(current)) { groups.set(current, []); order.push(current); }
  }
  if (order.length === 0) return null;
  let html = `<div class="dsh-search">`;
  for (const p of order) {
    html += `<div class="dsh-search-file">${esc(p)}</div>`;
    for (const h of groups.get(p)) {
      html += `<div class="dsh-search-line"><span class="dsh-search-gutter" aria-hidden>${esc(h.n)}</span><span class="dsh-search-text">${esc(h.t)}</span></div>`;
    }
  }
  html += `</div>`;
  return html;
}


// ---- message stream --------------------------------------------------------
// addUserMsg renders a user bubble; images (P5) is an optional list of
// {src, id} thumbnails shown above the text. The actions row (copy + clock)
// sits BELOW the bubble and is hover-revealed, exactly like dsh
// (UserStyleBubble + MessageIconActions with data-time-hover-root).
function addUserMsg(text, timeIso, images) {
  const inner = msgInner();
  const node = document.createElement("div");
  node.className = "msg user";
  let imgs = "";
  if (images && images.length) {
    const cls = images.length === 1 ? "single" : "multi";
    imgs = `<div class="msg-images ${cls}">${images.map((im) => `<img class="msg-image" src="${esc(im.src)}" alt="图片" loading="lazy">`).join("")}</div>`;
  }
  node.innerHTML = `${imgs}<div class="bubble">${esc(text)}</div>
    <div class="actions-row">
      <button class="act-btn" data-act="copy" title="复制" aria-label="复制">${DSH_ICON_COPY}</button>
      <span class="act-time">${fmtTime(timeIso)}</span>
    </div>`;
  node.querySelector('[data-act="copy"]').addEventListener("click", () => {
    navigator.clipboard?.writeText(text || "").catch(() => {});
  });
  // failed history images retry once with a cache-busting query
  node.querySelectorAll(".msg-image").forEach((img) => {
    img.addEventListener("error", () => {
      if (img.dataset.retried) return;
      img.dataset.retried = "1";
      img.src = img.src.split("?")[0] + "?retry=" + Date.now();
    });
  });
  inner.appendChild(node);
  scrollToBottom(true);
}

// formatRunDuration renders a localized elapsed label (dsh
// formatRunDuration): whole seconds under a minute, "M分SS秒" beyond.
function formatRunDuration(ms) {
  const total = Math.max(0, Math.floor(ms / 1000));
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return minutes > 0 ? `${minutes}分${String(seconds).padStart(2, "0")}秒` : `${seconds}秒`;
}

// updateRunningClock refreshes the turn-level elapsed clock. Short turns keep
// the plain label; the clock only appears once the turn has clearly been
// running for a while (dsh: showClock after 15s).
function updateRunningClock() {
  if (!runningNode) return;
  const clock = runningNode.querySelector(".running-clock");
  if (!clock) return;
  const elapsed = Date.now() - runningStart;
  clock.textContent = elapsed >= 15000 ? formatRunDuration(elapsed) : "";
}

// addRunning shows the turn-level loading signal in the FIXED #turn-status
// seat above the input panel (dsh TurnStatus — a sibling of the scrolling
// transcript, so it never scrolls away). It is NOT removed per step: it rides
// the whole running turn (first-token wait, tool execution, streaming) so it
// never flickers between steps; the clock ticks once a second and appears
// after 15s.
function addRunning() {
  if (runningNode) return;
  const el = document.getElementById("turn-status");
  if (!el) return;
  el.classList.remove("hidden");
  el.innerHTML = `<div class="running-text" role="status">Deep diving...<span class="running-clock" aria-hidden></span></div>`;
  runningNode = el;
  runningStart = Date.now();
  updateRunningClock();
  runningTimer = setInterval(updateRunningClock, 1000);
  scrollToBottom(true); // the turn's first events land below the last message
}
function removeRunning() {
  if (runningTimer) { clearInterval(runningTimer); runningTimer = null; }
  if (runningNode) {
    runningNode.classList.add("hidden");
    runningNode.innerHTML = "";
    runningNode = null;
  }
}

function addAssistant(text, timeIso, seq) {
  const inner = msgInner();
  const node = document.createElement("div");
  node.className = "msg assistant";
  // dsh IconActions chrome: monochrome (currentColor) SVG glyphs for copy /
  // like / dislike — no emoji, no colored thumbs.
  const fb = seq != null
    ? `<button class="act-btn" data-act="up" title="好" aria-label="好">${DSH_ICON_LIKE}</button>
       <button class="act-btn" data-act="down" title="差" aria-label="差">${DSH_ICON_DISLIKE}</button>`
    : "";
  node.innerHTML = `
    <div class="markdown">${text ? renderMarkdown(text) : "<p></p>"}</div>
    <div class="actions-row">
      <button class="act-btn" data-act="copy" title="复制" aria-label="复制">${DSH_ICON_COPY}</button>
      ${fb}
      <span class="act-time">${fmtTime(timeIso)}</span>
    </div>`;
  node.querySelector('[data-act="copy"]').addEventListener("click", () => {
    navigator.clipboard?.writeText(text || "").catch(() => {});
  });
  if (seq != null) {
    const feedbackSession = currentID;
    const feedbackSeq = seq;
    const upBtn = node.querySelector('[data-act="up"]');
    const downBtn = node.querySelector('[data-act="down"]');
    const setButtons = (rating) => {
      upBtn.classList.toggle("active-up", rating === "positive");
      downBtn.classList.toggle("active-down", rating === "negative");
    };
    setButtons(feedbackBySeq.get(feedbackSeq) || "");
    const saveFeedback = async (rating) => {
      const current = feedbackBySeq.get(feedbackSeq) || "";
      const next = current === rating ? "" : rating;
      upBtn.disabled = true;
      downBtn.disabled = true;
      try {
        const path = `/api/sessions/${encodeURIComponent(feedbackSession)}/feedback/${encodeURIComponent(feedbackSeq)}`;
        const res = next
          ? await api(path, { method: "PUT", body: JSON.stringify({ rating: next }) })
          : await api(path, { method: "DELETE" });
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          throw new Error(body.error || ("HTTP " + res.status));
        }
        if (currentID === feedbackSession) {
          if (next) feedbackBySeq.set(feedbackSeq, next);
          else feedbackBySeq.delete(feedbackSeq);
          setButtons(next);
        }
      } catch (e) {
        if (e.message !== "unauthorized") {
          toast("反馈保存失败");
          console.error("feedback", e);
        }
      } finally {
        upBtn.disabled = false;
        downBtn.disabled = false;
      }
    };
    upBtn.addEventListener("click", () => saveFeedback("positive"));
    downBtn.addEventListener("click", () => saveFeedback("negative"));
  }
  inner.appendChild(node);
  return node.querySelector(".markdown");
}

// appendAssistantStreaming: mutate the live assistant bubble with chunk text.
function appendAssistantStreaming(chunk, seq) {
  let md = streamState && streamState.node;
  if (!md) {
    // The running indicator stays: it is turn-level (dsh TurnStatus) and only
    // goes away when the turn settles (sendMessage's finally).
    const node = addAssistant("", null, seq);
    streamState = { node };
  }
  streamState.node.append(esc(chunk));
  scrollToBottom(true);
}
function finishAssistant(text, timeIso, seq) {
  if (streamState && streamState.node) {
    if (text) {
      // replace accumulated DOM text with the final rendered markdown
      streamState.node.innerHTML = renderMarkdown(text);
    } else {
      // Reasoning-only step (empty final text, e.g. the model only thought and
      // called tools): drop the empty bubble — the Think row is the step's
      // visible content (dsh), so no stray copy button / timestamp shows.
      streamState.node.remove();
    }
    streamState = null;
  } else if (text) {
    // replay path (snapshot with no streaming chunks): render the bubble fresh
    addAssistant(text, timeIso, seq);
  }
  // The running/stop state is NOT reset here: in a multi-step turn this fires
  // once per step while tool calls and further steps still execute. The turn
  // state is cleared when the POST /message settles (sendMessage's finally),
  // so the composer keeps the STOP affordance for the whole turn (dsh).
  scrollToBottom(true);
}

function addErrorRow(ev) {
  const inner = msgInner();
  const node = document.createElement("div");
  node.className = "msg error";
  node.innerHTML = `<div class="error-row"><span class="error-dot"></span><span>本轮运行失败：${esc(ev.summary || "")}</span></div>`;
  inner.appendChild(node);
  scrollToBottom(true);
}

// addContextInjection renders one context-injection step (dsh 上下文注入):
// recall hits, skill catalog, compaction summary — logged before the turn's
// first model request.
function addContextInjection(ev) {
  const inner = msgInner();
  const node = document.createElement("div");
  node.className = "msg context";
  node.innerHTML = `<div class="context-row"><span class="context-dot"></span><span>${esc(ev.summary || "上下文注入")}</span></div>`;
  inner.appendChild(node);
  scrollToBottom(true);
}

// addCommandRow mirrors dsh GenericCommandCard: API glyph at rest, the dsh
// disclosure glyph on hover/focus, a bare command name, a 2px separator and
// the handler-authored result. Multiline results stay compact on the row and
// are available in a collapsed preformatted body.
function addCommandRow(ev) {
  const inner = msgInner();
  const command = String(ev.command || "command").replace(/^\/+/, "") || "command";
  const text = String(ev.summary || "");
  const failed = /^\s*⚠/.test(text);
  const body = text.includes("\n") ? text : "";
  const summary = firstLine(text || (failed ? "命令失败" : "命令已完成"));
  // /help is the command-discovery surface. dsh keeps the catalog visible in
  // its composer, so the equivalent Web result opens its complete catalog
  // immediately; other multiline command results retain the compact card.
  const expanded = command === "help" && body !== "";
  const node = document.createElement("div");
  node.className = "msg command";
  node.dataset.command = command;
  if (ev.seq != null) node.dataset.seq = String(ev.seq);
  node.innerHTML = `<div class="command-row">
    <button type="button" class="command-toggle" aria-expanded="${expanded ? "true" : "false"}"${body ? "" : " disabled"}>
      <span class="command-leading" aria-hidden="true">
        <span class="command-context-icon">${failed ? '<span class="dsh-statedot dsh-statedot-err"></span>' : DSH_ICON_API}</span>
        <span class="command-disclosure-icon">${expanded ? DSH_ICON_CHEVRON_DOWN : DSH_ICON_CHEVRON_RIGHT}</span>
      </span>
      <span class="command-title">${esc(command)}</span>
      <span class="command-sep" aria-hidden="true"></span>
      <span class="command-summary${failed ? " command-summary-err" : ""}">${esc(summary)}</span>
    </button>
    ${body ? (command === "help"
      ? `<div class="command-body command-help-body${expanded ? "" : " hidden"}">${renderHelpBody(body)}</div>`
      : `<pre class="command-body${expanded ? "" : " hidden"}">${esc(body)}</pre>`) : ""}
  </div>`;
  const button = node.querySelector(".command-toggle");
  if (body) {
    button.addEventListener("click", () => {
      const content = node.querySelector(".command-body");
      const open = content.classList.toggle("hidden");
      button.setAttribute("aria-expanded", String(!open));
      const disclosure = node.querySelector(".command-disclosure-icon");
      disclosure.innerHTML = open ? DSH_ICON_CHEVRON_RIGHT : DSH_ICON_CHEVRON_DOWN;
    });
  }
  inner.appendChild(node);
  scrollToBottom(true);
  return node;
}

// dsh compaction presentation: the replacement user/message is not shown as
// a normal user bubble. Its lifecycle is folded into one collapsed row, with
// the generated summary available on demand and the saved-token count filled
// when compaction/end arrives.
let pendingCompactionRow = null;
const compactionRows = new Map();
function resetCompactionRows() {
  pendingCompactionRow = null;
  compactionRows.clear();
  pendingSlashCommands = [];
}
function compactionRowMeta(ev) {
  if (ev.compaction_error) return `压缩失败：${ev.compaction_error}`;
  if (ev.type === "compaction/end") {
    return `已压缩 ${ev.compaction_items || 0} 条历史记录（约 ${fmtTokens(ev.compaction_tokens || 0)} tokens）`;
  }
  if (ev.compaction_items && ev.compaction_tokens) {
    return `已压缩 ${ev.compaction_items} 条历史记录（约 ${fmtTokens(ev.compaction_tokens)} tokens）`;
  }
  if (ev.compaction_tokens) {
    return `已释放约 ${fmtTokens(ev.compaction_tokens)} tokens`;
  }
  return "正在压缩上下文…";
}
function compactionRowTitle(ev) {
  return "compact";
}
function ensureCompactionRow(ev) {
  let node = ev.compaction_id ? compactionRows.get(ev.compaction_id) : pendingCompactionRow;
  if (node) return node;
  const inner = msgInner();
  node = document.createElement("div");
  node.className = "msg context compaction";
  node.innerHTML = `<div class="context-row compaction-row">
    <button type="button" class="compaction-toggle" aria-expanded="false">
      <span class="compaction-leading" aria-hidden="true">
        <span class="compaction-context-icon" data-compaction-icon="context">${DSH_ICON_API}</span>
        <span class="compaction-disclosure-icon" data-compaction-disclosure="collapsed">${DSH_ICON_CHEVRON_RIGHT}</span>
      </span>
      <span class="compaction-title">${esc(compactionRowTitle(ev))}</span>
      <span class="compaction-sep" aria-hidden="true"></span>
      <span class="compaction-meta">${esc(compactionRowMeta(ev))}</span>
    </button>
    <div class="compaction-body hidden"></div>
  </div>`;
  node.querySelector(".compaction-toggle").addEventListener("click", () => {
    const body = node.querySelector(".compaction-body");
    const button = node.querySelector(".compaction-toggle");
    const open = body.classList.toggle("hidden");
    button.setAttribute("aria-expanded", String(!open));
    const disclosure = node.querySelector(".compaction-disclosure-icon");
    if (disclosure) {
      disclosure.dataset.compactionDisclosure = open ? "collapsed" : "expanded";
      disclosure.innerHTML = open ? DSH_ICON_CHEVRON_RIGHT : DSH_ICON_CHEVRON_DOWN;
    }
  });
  inner.appendChild(node);
  pendingCompactionRow = node;
  scrollToBottom(true);
  return node;
}
function addCompactionEvent(ev) {
  const node = ensureCompactionRow(ev);
  const id = ev.compaction_id || "";
  if (id) {
    compactionRows.set(id, node);
    pendingCompactionRow = null;
  }
  const title = node.querySelector(".compaction-title");
  if (title && ev.type === "compaction/end") title.textContent = compactionRowTitle(ev);
  const meta = node.querySelector(".compaction-meta");
  if (ev.type === "compaction/end" && meta) meta.textContent = compactionRowMeta(ev);
  const body = node.querySelector(".compaction-body");
  const summary = ev.compaction_summary || (ev.compaction_marker ? ev.summary : "");
  if (summary && body && !body.dataset.summary) {
    body.innerHTML = renderMarkdown(summary);
    body.dataset.summary = "1";
    const button = node.querySelector(".compaction-toggle");
    button.disabled = false;
    button.title = "点击查看压缩摘要";
    meta.textContent = ev.type === "compaction/end" ? compactionRowMeta(ev) : "点击查看压缩摘要";
  }
  return node;
}

// ---- scroll behavior -------------------------------------------------------
function nearBottom() {
  return messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight <= 24;
}
function scrollToBottom(force) {
  if (force || nearBottom()) {
    messagesEl.scrollTop = messagesEl.scrollHeight;
    scrollBottomBtn.classList.add("hidden");
  } else {
    scrollBottomBtn.classList.remove("hidden");
  }
}
messagesEl.addEventListener("scroll", () => {
  if (nearBottom()) scrollBottomBtn.classList.add("hidden");
  else scrollBottomBtn.classList.remove("hidden");
});
scrollBottomBtn.addEventListener("click", () => { scrollToBottom(true); });

// ---- session list (P2: dsh ui-workspace single-line rows) -------------------
let searchQuery = "";
let streamActive = false; // a streaming assistant turn is in flight
let turnRunning = false;  // the composer shows the ■ stop affordance (dsh 停止)
// dsh ui-workspace search affordance: a section-header icon that expands into
// an inline input (wide), and a 36px rail icon that expands the sidebar and
// lands focus in the input (rail). A non-empty query pins the expansion open.
const wsSearchEl = $("ws-search"), searchToggle = $("search-toggle"),
  searchInput = $("session-search"), searchClear = $("search-clear");
function setSearchExpanded(on) {
  wsSearchEl.classList.toggle("expanded", on);
  searchClear.hidden = !on;
  if (on) { searchInput.tabIndex = 0; searchInput.focus(); }
  else { searchInput.tabIndex = -1; searchInput.blur(); }
}
searchToggle.addEventListener("click", (e) => {
  e.stopPropagation();
  if (sidebarCollapsed()) toggleSidebar();   // rail gesture: expand first
  setSearchExpanded(true);
});
let searchTimer = null;
searchInput.addEventListener("input", (e) => {
  searchQuery = e.target.value.trim();
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => loadSessions(), 250); // debounce (dsh remote search)
});
searchClear.addEventListener("click", (e) => {
  e.stopPropagation();
  searchQuery = ""; searchInput.value = "";
  loadSessions();
  setSearchExpanded(false);
});
searchInput.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  e.stopPropagation();
  searchQuery = ""; searchInput.value = "";
  loadSessions();
  setSearchExpanded(false);
});
document.addEventListener("click", (e) => {
  if (!wsSearchEl.classList.contains("expanded")) return;
  if (e.target.closest("#ws-search")) return;
  if (searchQuery) return; // keep a live filter pinned
  setSearchExpanded(false);
});

// fmtShort: sidebar relative/compact time (dsh ui-workspace relativeTime).
function fmtShort(iso) {
  return relTime(iso);
}
// dsh ui-workspace relativeTime buckets: just-now / minutes / hours / days /
// months / years. The row label is compact ("5分钟", no 前) and the hover label
// wraps in the ago template ("5分钟前"); the now bucket stays bare in both.
const REL_UNIT_ZH = { minutes: "分钟", hours: "小时", days: "天", months: "个月", years: "年" };
function relBucket(iso, nowMs) {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return null;
  const diff = Math.max(0, nowMs - d.getTime());
  const MIN = 60000, HOUR = 3600000, DAY = 86400000;
  if (diff < MIN) return { unit: "now", n: 0 };
  if (diff < HOUR) return { unit: "minutes", n: Math.floor(diff / MIN) };
  if (diff < DAY) return { unit: "hours", n: Math.floor(diff / HOUR) };
  if (diff < 30 * DAY) return { unit: "days", n: Math.floor(diff / DAY) };
  if (diff < 365 * DAY) return { unit: "months", n: Math.floor(diff / (30 * DAY)) };
  return { unit: "years", n: Math.floor(diff / (365 * DAY)) };
}
function relTime(iso) {
  const b = relBucket(iso, Date.now());
  if (!b) return "";
  if (b.unit === "now") return "刚刚";
  return `${b.n}${REL_UNIT_ZH[b.unit]}`;
}
function relTimeAgo(iso) {
  const b = relBucket(iso, Date.now());
  if (!b) return "";
  if (b.unit === "now") return "刚刚";
  return `${b.n}${REL_UNIT_ZH[b.unit]}前`;
}

// ---- P6 workspace grouping (dsh grouped sidebar view) ----------------------
// groupBy is persisted in localStorage like dsh's store; grouped is the
// default (dsh ships grouped). orderBy (manual/updated) mirrors dsh's
// ViewOptionsMenu sort mode. wsGroupOpen remembers per-group collapse.
const GROUP_SESSION_LIMIT = 5;
let groupBy = localStorage.getItem("pa_groupby") || "workspace";
let orderBy = localStorage.getItem("pa_orderby") || "manual";
let wsGroups = [];      // [{id,title,session_ids}]
let wsUngrouped = [];   // ungrouped session ids
let lastSessionList = []; // most recent /api/sessions payload (workspace_id per session)
let wsGroupOpen = {};
// wsOpenState reads a group's expansion. Default COLLAPSED (dsh
// groupExpansion: {} — the current session's group auto-expands instead,
// see autoExpandCurrentGroup); "1" stored = expanded.
function wsOpenState(key) {
  if (!(key in wsGroupOpen)) wsGroupOpen[key] = localStorage.getItem("pa_ws_g:" + key) === "1";
  return wsGroupOpen[key];
}
function setWsOpen(key, open) {
  wsGroupOpen[key] = open;
  localStorage.setItem("pa_ws_g:" + key, open ? "1" : "0");
}
// dateBucketOf returns the date-view bucket label for an ISO timestamp
// (renderDateGroups' 今天/昨天/最近 7 天/最近 30 天/更早 boundaries).
function dateBucketOf(iso) {
  const day = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  const today = day(new Date());
  const DAY = 86400000;
  const t = new Date(iso).getTime();
  if (t >= today) return "今天";
  if (t >= today - DAY) return "昨天";
  if (t >= today - 6 * DAY) return "最近 7 天";
  if (t >= today - 29 * DAY) return "最近 30 天";
  return "更早";
}
// autoExpandCurrentGroup starts the current session's group expanded when the
// user has no stored preference for it (dsh: the group holding the current
// session expands on mount while unset).
function autoExpandCurrentGroup(list, view) {
  if (!currentID || !Array.isArray(list)) return;
  const cur = list.find((x) => x.id === currentID);
  if (!cur) return;
  let ckey = "";
  if (view === "workspace") {
    const w = wsGroups.find((g) => g.session_ids.includes(currentID));
    ckey = w ? w.id : (wsUngrouped.includes(currentID) ? "__u" : "");
  } else {
    ckey = "d:" + dateBucketOf(cur.updated_at);
  }
  if (ckey && localStorage.getItem("pa_ws_g:" + ckey) === null) {
    wsGroupOpen[ckey] = true;
    localStorage.setItem("pa_ws_g:" + ckey, "1");
  }
}

async function loadSessions() {
  // dsh section label: 工作区 in grouped views, 会话 in the flat list.
  const label = $("ws-label");
  if (label) label.textContent = groupBy === "flat" ? "会话" : "工作区";
  let res;
  try {
    res = await api("/api/sessions");
  } catch (e) { if (e.message !== "unauthorized") console.error(e); return; }
  const list = await res.json();
  sessionList.textContent = "";
  closeAnyMenu();
  if (!Array.isArray(list) || list.length === 0) {
    const li = document.createElement("li");
    li.className = "session-item";
    li.innerHTML = `<span class="si-title empty">还没有会话，点「新会话」开始</span>`;
    sessionList.appendChild(li);
    return;
  }
  list.sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at));
  // Persist the active session's workspace (dsh recentWorkspaceId) so the next
  // 新会话 lands in the same workspace even without an active session; the
  // topbar title follows the freshly derived session title.
  rememberRecentWorkspace(list);
  syncSessionTitle();
  // A live query switches to the remote body-text search view (P6.3, dsh
  // searchAcrossSessions); nothing else is drawn while searching.
  if (searchQuery) { doSearch(searchQuery); return; }
  if (groupBy === "flat") { renderFlat(list, ""); return; }
  if (groupBy === "date") {
    autoExpandCurrentGroup(list, "date");
    renderDateGroups(list);
    return;
  }
  try {
    const wr = await api("/api/workspaces");
    const data = await wr.json();
    wsGroups = data.workspaces || [];
    wsUngrouped = data.ungrouped_ids || [];
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
  autoExpandCurrentGroup(list, "workspace");
  renderGrouped(list);
}

// doSearch fetches body-text hits and draws the search-results view.
async function doSearch(q) {
  let res;
  try {
    res = await api("/api/search?q=" + encodeURIComponent(q));
  } catch (e) { if (e.message !== "unauthorized") console.error(e); return; }
  const data = await res.json();
  const hits = data.hits || [];
  sessionList.textContent = "";
  if (hits.length === 0) {
    const li = document.createElement("li");
    li.className = "session-item";
    li.innerHTML = `<span class="si-title empty">没有找到「${esc(q)}」相关的会话</span>`;
    sessionList.appendChild(li);
    return;
  }
  for (const h of hits) {
    const li = document.createElement("li");
    li.className = "session-item search-hit";
    li.dataset.id = h.id;
    const title = h.title || truncate(h.snippet, 18) || "会话";
    li.innerHTML = `
      <span class="si-dot" data-state="done"></span>
      <span class="sh-main">
        <span class="si-title">${highlight(title, q)}</span>
        <span class="sh-snippet">${highlight(h.snippet, q)}</span>
      </span>
      <span class="si-time">${fmtShort(h.updated_at)}</span>`;
    li.addEventListener("click", () => switchSession(h.id));
    sessionList.appendChild(li);
  }
}

// truncate shortens a string to n chars with an ellipsis (search fallback title).
function truncate(s, n) { return s && s.length > n ? s.slice(0, n) + "…" : s; }
// highlight wraps every case-insensitive occurrence of q in <mark>.
function highlight(text, q) {
  const escT = esc(text || "");
  if (!q) return escT;
  const escQ = esc(q).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return escT.replace(new RegExp(escQ, "gi"), (m) => `<mark>${m}</mark>`);
}

// injectIcons fills every [data-icon] element with the dsh web SVG glyphs
// (icons.js holds the exact paths — user requested the same icons as dsh web).
function injectIcons() {
  const icons = window.PA_ICONS || {};
  document.querySelectorAll("[data-icon]").forEach((el) => {
    const svg = icons[el.dataset.icon];
    if (svg) el.innerHTML = svg;
  });
}

// sessionState resolves a row's dot state from the backend status (dsh
// session-status): idle (no dot) / done / ongoing / warning. When the status
// is absent (older API) it falls back to the pre-status blank/done heuristic.
function sessionState(s) {
  if (s.status && s.status.state) return s.status.state;
  return s.blank ? "idle" : "done";
}
// statusLabel picks the primary human status label for the row + hover card.
function statusLabel(s) {
  if (s.status && s.status.statuses && s.status.statuses.length) {
    return s.status.statuses[0].label;
  }
  return sessionState(s) === "idle" ? "空闲" : "已完成";
}

// appendSessionItem appends one session row into a container (shared by the
// flat list and the grouped sublists). dsh SessionNodeItem row contract: a
// blank (no prompt yet) session shows the localized New Session label and no
// timestamp or rename/fork/archive verbs — only the destructive delete remains.
// The status dot reflects the live backend state, and hovering reveals a card
// with the full title, relative time and every status (dsh hover card).
function appendSessionItem(container, s) {
  const state = sessionState(s);
  const title = s.title || "新会话";
  const li = document.createElement("li");
  li.className = "session-item" + (s.id === currentID ? " active" : "");
  li.dataset.id = s.id;
  li.dataset.idle = state === "idle" ? "1" : "";
  li.draggable = true;
  li.setAttribute("aria-label", (title + " " + statusLabel(s)).trim());
  // dsh SessionNodeItem row contract: a blank (no prompt yet) session shows
  // the New Session label and NO trailing cells — no timestamp, no row verbs
  // (rename/fork/archive); the status dot follows the dsh rule (idle = no
  // visible dot, the fixed slot keeps alignment).
  li.innerHTML = `
    <span class="si-dot" data-state="${state}"></span>
    <span class="si-title${s.blank ? " empty" : ""}">${esc(title)}</span>
    ${s.blank ? "" : `<span class="si-time">${fmtShort(s.updated_at)}</span>`}
    ${s.blank ? "" : `<button class="si-menu" title="会话操作">${PA_ICONS.ellipsis}</button>`}`;
  li.addEventListener("click", (e) => {
    if (e.target.closest(".si-menu")) return;
    switchSession(s.id);
  });
  const menuBtn = li.querySelector(".si-menu");
  if (menuBtn) menuBtn.addEventListener("click", (e) => {
    e.stopPropagation();
    openMenu(li, s);
  });
  // dsh hover card: shows while the pointer is over the row, suppressed when a
  // row menu is open (openMenuEl set).
  li.addEventListener("mouseenter", () => { if (!openMenuEl) showSessionHover(li, s); });
  li.addEventListener("mousemove", () => { if (!openMenuEl) positionSessionHover(li); });
  li.addEventListener("mouseleave", hideSessionHover);
  container.appendChild(li);
}

// ---- dsh hover card -------------------------------------------------------
// si-hover is a portal-style floating card (fixed, appended to body) shown over
// the row: full title, relative time (…前 variant), every live status and a
// copy-title button. Mirrors dsh SessionHoverContent.
let siHoverEl = null;
function ensureSiHover() {
  if (siHoverEl) return siHoverEl;
  siHoverEl = document.createElement("div");
  siHoverEl.id = "si-hover";
  siHoverEl.className = "si-hover";
  siHoverEl.hidden = true;
  document.body.appendChild(siHoverEl);
  return siHoverEl;
}
function showSessionHover(rowEl, s) {
  const card = ensureSiHover();
  const title = s.title || "新会话";
  const statuses = (s.status && s.status.statuses) || [];
  const statusHtml = statuses
    .map((st) => `<div class="shv-status"><span class="shv-dot" data-state="${st.state}"></span><span>${esc(st.label)}</span></div>`)
    .join("");
  card.innerHTML = `
    <div class="shv-title">${esc(title)}</div>
    ${s.blank ? "" : `<div class="shv-time">${relTimeAgo(s.updated_at)}</div>`}
    ${statusHtml || `<div class="shv-status"><span class="shv-dot" data-state="idle"></span><span>空闲</span></div>`}
    <button type="button" class="shv-copy">复制</button>`;
  const copyBtn = card.querySelector(".shv-copy");
  if (copyBtn) {
    copyBtn.addEventListener("click", () => {
      const text = s.title || "";
      if (!text) return;
      (navigator.clipboard && navigator.clipboard.writeText)
        ? navigator.clipboard.writeText(text)
        : (copyBtn.textContent = "已复制");
      if (navigator.clipboard) copyBtn.textContent = "已复制";
      setTimeout(() => { copyBtn.textContent = "复制"; }, 1200);
    });
  }
  card.hidden = false;
  positionSessionHover(rowEl);
}
function positionSessionHover(rowEl) {
  const card = ensureSiHover();
  if (card.hidden) return;
  const r = rowEl.getBoundingClientRect();
  const cw = card.offsetWidth, ch = card.offsetHeight;
  let left = r.right + 8;
  if (left + cw > window.innerWidth - 8) left = Math.max(8, r.left - cw - 8);
  let top = r.top;
  if (top + ch > window.innerHeight - 8) top = Math.max(8, window.innerHeight - ch - 8);
  card.style.left = left + "px";
  card.style.top = top + "px";
}
function hideSessionHover() {
  if (siHoverEl) siHoverEl.hidden = true;
}

// ---- workspace header hover card (dsh ProjectRowItem) ----------------------
// A portal card over a workspace group header: title + creation time. shutu
// workspaces are title-only groups (no persisted directory path), so the dsh
// cwd line is intentionally omitted.
let wsHoverEl = null;
function ensureWsHover() {
  if (wsHoverEl) return wsHoverEl;
  wsHoverEl = document.createElement("div");
  wsHoverEl.id = "ws-hover";
  wsHoverEl.className = "ws-hover";
  wsHoverEl.hidden = true;
  document.body.appendChild(wsHoverEl);
  return wsHoverEl;
}
function showWorkspaceHover(rowEl, g) {
  const card = ensureWsHover();
  const created = g.created_at;
  const timeHtml = created ? `<div class="shv-time">创建于 ${fmtShort(created)}</div>` : "";
  const cwdHtml = g.path ? `<div class="shv-cwd">${esc(g.path)}</div>` : "";
  card.innerHTML = `
    <div class="shv-title">${esc(g.title)}</div>
    ${timeHtml}${cwdHtml}`;
  card.hidden = false;
  positionWorkspaceHover(rowEl);
}
function positionWorkspaceHover(rowEl) {
  const card = ensureWsHover();
  if (card.hidden) return;
  const r = rowEl.getBoundingClientRect();
  const cw = card.offsetWidth, ch = card.offsetHeight;
  let left = r.right + 8;
  if (left + cw > window.innerWidth - 8) left = Math.max(8, r.left - cw - 8);
  let top = r.top;
  if (top + ch > window.innerHeight - 8) top = Math.max(8, window.innerHeight - ch - 8);
  card.style.left = left + "px";
  card.style.top = top + "px";
}
function hideWorkspaceHover() {
  if (wsHoverEl) wsHoverEl.hidden = true;
}

// renderFlat draws the single-list view (dsh FlatList). In manual order a
// user drag establishes flat_sort (manual order wins); otherwise fall back to
// the recently-updated list order. In updated order recent activity always wins.
function renderFlat(list) {
  let arr = list;
  if (orderBy === "manual") {
    const hasManual = list.some((s) => s.flat_sort > 0);
    if (hasManual) arr = [...list].sort((a, b) => a.flat_sort - b.flat_sort);
  }
  for (const s of arr) appendSessionItem(sessionList, s);
}

// renderGrouped draws the dsh grouped tree: a workspace header row per group
// (folder + title + hover add/menu, dsh ProjectRowItem) then its session
// rows; a group collapses to its header, and more than GROUP_SESSION_LIMIT
// rows collapse to a 5-row run plus an "expand all" button. The ungrouped
// bucket keeps its sessions but has no workspace actions.
function renderGrouped(list) {
  const byId = new Map(list.map((s) => [s.id, s]));
  const groups = [];
  for (const w of wsGroups) {
    const ids = w.session_ids.filter((id) => byId.has(id));
    // dsh orderBy=updated re-sorts rows by recent activity inside each group;
    // manual order keeps the backend's sort (workspace Sort asc) as-is.
    if (orderBy === "updated") {
      ids.sort((a, b) => new Date(byId.get(b).updated_at) - new Date(byId.get(a).updated_at));
    }
    groups.push({ key: w.id, title: w.title, ws: true, ids, created_at: w.created_at });
  }
  const unIds = wsUngrouped.filter((id) => byId.has(id));
  if (unIds.length > 0) groups.push({ key: "__u", title: "未分组", ws: false, ids: unIds });
  let any = false;
  for (const g of groups) {
    if (g.ids.length === 0) continue;
    any = true;
    const wrap = document.createElement("div");
    wrap.className = "ws-group" + (wsOpenState(g.key) ? "" : " closed");
    wrap.dataset.key = g.key;
    const head = document.createElement("button");
    head.className = "group-head";
    head.draggable = true;
    const open = wsOpenState(g.key);
    // dsh ProjectRowItem folderActive: the group owning the current session
    // shows a business-blue folder icon while expanded — the ungrouped bucket
    // is a first-class group too (dsh containsCurrent falls back to
    // UNGROUPED_KEY), and its folder swaps open/close like any workspace.
    const folderActive = open && g.ids.includes(currentID);
    // dsh ProjectRowItem: folder + title + hover actions — NO session count
    // (dsh renders the count nowhere in the tree rows).
    head.innerHTML = `
      <span class="gh-chevron" aria-hidden="true">${PA_ICONS.triangleright}</span>
      <span class="gh-folder${folderActive ? " active" : ""}" aria-hidden="true">${open ? PA_ICONS.folderopen16 : PA_ICONS.folderclose16}</span>
      <span class="gh-title">${esc(g.title)}</span>
      ${g.ws ? `<span class="gh-actions">
        <span class="gh-act gh-add" title="在此新建会话">${PA_ICONS.plus}</span>
        <span class="gh-act gh-menu" title="工作区操作">${PA_ICONS.ellipsis}</span>
      </span>` : ""}`;
    head.addEventListener("click", (e) => {
      if (e.target.closest(".gh-add") || e.target.closest(".gh-menu")) return;
      const next = !wsOpenState(g.key);
      setWsOpen(g.key, next);
      wrap.classList.toggle("closed", !next);
      head.querySelector(".gh-folder").innerHTML = next ? PA_ICONS.folderopen16 : PA_ICONS.folderclose16;
      head.querySelector(".gh-folder").classList.toggle("active", next && g.ids.includes(currentID));
    });
    if (g.ws) {
      head.querySelector(".gh-add").addEventListener("click", async (e) => {
        e.stopPropagation();
        try {
          const res = await api("/api/sessions", {
            method: "POST", body: JSON.stringify({ workspace_id: g.key }),
          });
          const body = await res.json();
          localStorage.setItem(KEY_CURRENT, body.id);
          currentID = body.id;
          await openSession(body.id);
          loadSessions();
        } catch (err) { if (err.message !== "unauthorized") console.error(err); }
      });
      head.querySelector(".gh-menu").addEventListener("click", (e) => {
        e.stopPropagation();
        openWorkspaceMenu(g);
      });
      // dsh ProjectRowItem hover card: title + creation time (shutu workspaces
      // carry no directory path, so the cwd line is omitted).
      head.addEventListener("mouseenter", () => { if (!openMenuEl) showWorkspaceHover(head, g); });
      head.addEventListener("mousemove", () => { if (!openMenuEl) positionWorkspaceHover(head); });
      head.addEventListener("mouseleave", hideWorkspaceHover);
    }
    wrap.appendChild(head);
    // Rows are ALWAYS in the DOM; the .closed class hides them (dsh: clicking
    // the header toggles the group — a group rendered collapsed must still be
    // able to expand without a re-render).
    const ul = document.createElement("ul");
    ul.className = "group-sessions";
    const shown = g.ids.slice(0, GROUP_SESSION_LIMIT);
    for (const id of shown) appendSessionItem(ul, byId.get(id));
    if (g.ids.length > GROUP_SESSION_LIMIT) {
      const ob = document.createElement("button");
      ob.className = "session-overflow";
      ob.textContent = `展开全部会话（${g.ids.length}）`;
      ob.addEventListener("click", () => {
        for (const id of g.ids.slice(GROUP_SESSION_LIMIT)) appendSessionItem(ul, byId.get(id));
        ob.remove();
      });
      ul.appendChild(ob);
    }
    wrap.appendChild(ul);
    sessionList.appendChild(wrap);
  }
  if (!any) {
    const li = document.createElement("li");
    li.className = "session-item";
    li.innerHTML = `<span class="si-title empty">还没有会话，点「新会话」开始</span>`;
    sessionList.appendChild(li);
  }
}

// renderDateGroups draws the date-grouped tree (dsh groupBy=date): 今天 /
// 昨天 / 最近 7 天 / 最近 30 天 / 更早 buckets from updated_at. Buckets have
// no workspace actions — they are pure view grouping, collapsible like the
// workspace headers.
function renderDateGroups(list) {
  const byBucket = ["今天", "昨天", "最近 7 天", "最近 30 天", "更早"].map((key) => ({ key, ids: [] }));
  for (const s of list) {
    const b = byBucket.find((x) => x.key === dateBucketOf(s.updated_at));
    if (b) b.ids.push(s.id);
  }
  let any = false;
  for (const b of byBucket) {
    if (b.ids.length === 0) continue;
    any = true;
    const wrap = document.createElement("div");
    wrap.className = "ws-group" + (wsOpenState("d:" + b.key) ? "" : " closed");
    wrap.dataset.key = b.key;
    const head = document.createElement("button");
    head.className = "group-head";
    head.innerHTML = `
      <span class="gh-chevron" aria-hidden="true">${PA_ICONS.triangleright}</span>
      <span class="gh-folder" aria-hidden="true">${PA_ICONS.folderclose16}</span>
      <span class="gh-title">${esc(b.key)}</span>
      <span class="gh-count">${b.ids.length}</span>`;
    head.addEventListener("click", () => {
      setWsOpen("d:" + b.key, !wsOpenState("d:" + b.key));
      wrap.classList.toggle("closed", !wsOpenState("d:" + b.key));
    });
    wrap.appendChild(head);
    // Rows are always in the DOM; the .closed class hides them (same contract
    // as the workspace groups: a collapsed bucket can expand by clicking).
    const ul = document.createElement("ul");
    ul.className = "group-sessions";
    const byId = new Map(list.map((s) => [s.id, s]));
    for (const id of b.ids) appendSessionItem(ul, byId.get(id));
    wrap.appendChild(ul);
    sessionList.appendChild(wrap);
  }
  if (!any) {
    const li = document.createElement("li");
    li.className = "session-item";
    li.innerHTML = `<span class="si-title empty">还没有会话，点「新会话」开始</span>`;
    sessionList.appendChild(li);
  }
}
function openWorkspaceMenu(g) {
  closeAnyMenu();
  const pop = document.createElement("div");
  pop.className = "si-pop";
  pop.innerHTML = `
    <button data-act="rename">${PA_ICONS.edit}<span>重命名</span></button>
    <button data-act="delete" class="danger">${PA_ICONS.trash}<span>删除工作区</span></button>`;
  pop.addEventListener("click", (e) => {
    const act = e.target.closest("button")?.dataset.act;
    if (!act) return;
    closeAnyMenu();
    if (act === "rename") openWsDialog("rename", g.key, g.title);
    if (act === "delete") deleteWorkspace(g.key);
  });
  document.querySelector(`.ws-group[data-key="${CSS.escape(g.key)}"] .group-head`).appendChild(pop);
  openMenuEl = pop;
}

// ---- P6.2 drag & drop ordering (dsh DnD: workspace rows + session rows) ----
// HTML5 native DnD, no dependencies. Only the grouped view is draggable; the
// flat view keeps its updated_at order (honest boundary). A drop rewrites the
// whole target group's order via PATCH /api/sessions/order so the manual Sort
// is consistent, and dragging a session onto another group's header moves it.
let dragState = null; // {kind:'workspace'|'session', id, groupKey}
let dropPos = null;   // {kind, anchor, el, groupKey}
let dropInd = null;

function ensureDropInd() {
  if (!dropInd) {
    dropInd = document.createElement("div");
    dropInd.className = "drop-indicator";
    document.querySelector(".col-sidebar").appendChild(dropInd);
  }
  return dropInd;
}
function showDropInd(anchorEl, atBottom) {
  const ind = ensureDropInd();
  const col = document.querySelector(".col-sidebar");
  const cr = col.getBoundingClientRect();
  const r = anchorEl.getBoundingClientRect();
  ind.style.top = ((atBottom ? r.bottom : r.top) - cr.top - 2) + "px";
  ind.style.left = "0px";
  ind.style.width = cr.width + "px";
  ind.classList.add("visible");
}
function hideDropInd() { if (dropInd) dropInd.classList.remove("visible"); }

function onDragStart(e) {
  if (groupBy !== "workspace" && groupBy !== "flat") return;
  if (e.target.closest(".gh-act")) return;
  const head = e.target.closest(".group-head");
  const item = e.target.closest(".session-item");
  if (head && head.closest(".ws-group") && head.closest(".ws-group").dataset.key !== "__u") {
    dragState = { kind: "workspace", id: head.closest(".ws-group").dataset.key, groupKey: null };
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", dragState.id);
    return;
  }
  if (item) {
    dragState = {
      kind: "session",
      id: item.dataset.id,
      groupKey: groupBy === "workspace" ? (item.closest(".ws-group")?.dataset.key || "__u") : null,
    };
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", dragState.id);
  }
}

function onDragOver(e) {
  if (!dragState) return;
  e.preventDefault();
  e.dataTransfer.dropEffect = "move";
  const item = e.target.closest(".session-item");
  const head = e.target.closest(".group-head");
  if (dragState.kind === "session" && item) {
    const r = item.getBoundingClientRect();
    const before = e.clientY < r.top + r.height / 2;
    if (groupBy === "flat") {
      dropPos = { kind: "session-flat", anchor: before ? "before" : "after", el: item };
    } else {
      dropPos = { kind: "session", anchor: before ? "before" : "after", el: item, groupKey: item.closest(".ws-group")?.dataset.key || "__u" };
    }
    if (before) showDropInd(item);
    else if (item.nextElementSibling && item.nextElementSibling.classList.contains("session-item")) showDropInd(item.nextElementSibling);
    else showDropInd(item, true);
    return;
  }
  if (dragState.kind === "session" && head) {
    const gkey = head.closest(".ws-group").dataset.key;
    const first = head.parentElement.querySelector(".group-sessions .session-item");
    dropPos = { kind: "session", anchor: "top", el: first, groupKey: gkey };
    if (first) showDropInd(first);
    else showDropInd(head);
    return;
  }
  if (dragState.kind === "workspace" && head && head.closest(".ws-group").dataset.key !== "__u") {
    const wrap = head.closest(".ws-group");
    const r = head.getBoundingClientRect();
    const before = e.clientY < r.top + r.height / 2;
    dropPos = { kind: "workspace", anchor: before ? "before" : "after", el: wrap };
    const next = wrap.nextElementSibling;
    if (before) showDropInd(head);
    else if (next && next.classList.contains("ws-group")) showDropInd(next.querySelector(".group-head"));
    else showDropInd(wrap, true);
    return;
  }
  hideDropInd();
  dropPos = null;
}

async function onDrop(e) {
  e.preventDefault();
  const d = dragState;
  const pos = dropPos;
  hideDropInd();
  dragState = null;
  dropPos = null;
  if (!d || !pos) return;
  try {
    if (d.kind === "workspace" && pos.kind === "workspace") {
      const order = [...sessionList.querySelectorAll(".ws-group")]
        .map((w) => w.dataset.key)
        .filter((k) => k !== "__u");
      const from = order.indexOf(d.id);
      if (from === -1) return;
      order.splice(from, 1);
      const to = order.indexOf(pos.el.dataset.key);
      order.splice(to + (pos.anchor === "after" ? 1 : 0), 0, d.id);
      await api("/api/workspaces/order", { method: "PATCH", body: JSON.stringify({ ids: order }) });
      loadSessions();
      return;
    }
    if (d.kind === "session" && pos.kind === "session-flat") {
      const visible = [...sessionList.querySelectorAll(".session-item")].map((li) => li.dataset.id);
      const at = visible.indexOf(pos.el.dataset.id);
      let idx = pos.anchor === "after" ? at + 1 : at;
      const newOrder = visible.filter((id) => id !== d.id);
      if (at !== -1 && visible.indexOf(d.id) !== -1 && visible.indexOf(d.id) < at) idx -= 1;
      if (idx < 0) idx = 0;
      newOrder.splice(idx, 0, d.id);
      await api("/api/sessions/flat-order", { method: "PATCH", body: JSON.stringify({ ids: newOrder }) });
      loadSessions();
      return;
    }
    if (d.kind === "session") {
      const gkey = pos.groupKey;
      const gEl = sessionList.querySelector(`.ws-group[data-key="${CSS.escape(gkey)}"]`);
      const visible = gEl ? [...gEl.querySelectorAll(".session-item")].map((li) => li.dataset.id) : [];
      const shown = new Set(visible);
      let tail = [];
      if (gkey !== "__u") {
        const w = wsGroups.find((x) => x.id === gkey);
        if (w) tail = w.session_ids.filter((id) => !shown.has(id));
      } else {
        tail = wsUngrouped.filter((id) => !shown.has(id));
      }
      const at = pos.el ? visible.indexOf(pos.el.dataset.id) : -1;
      let idx = pos.anchor === "after" ? at + 1 : at;
      if (pos.anchor === "top") idx = 0;
      const newOrder = visible.filter((id) => id !== d.id);
      if (at !== -1 && visible.indexOf(d.id) !== -1 && visible.indexOf(d.id) < at) idx -= 1;
      if (idx < 0) idx = 0;
      newOrder.splice(idx, 0, d.id);
      const full = newOrder.concat(tail);
      await api("/api/sessions/order", {
        method: "PATCH",
        body: JSON.stringify({ workspace_id: gkey === "__u" ? "" : gkey, session_ids: full }),
      });
      loadSessions();
    }
  } catch (err) { if (err.message !== "unauthorized") console.error(err); }
}

function onDragEnd() {
  hideDropInd();
  dragState = null;
  dropPos = null;
}
sessionList.addEventListener("dragstart", onDragStart);
sessionList.addEventListener("dragover", onDragOver);
sessionList.addEventListener("drop", onDrop);
sessionList.addEventListener("dragend", onDragEnd);

async function deleteWorkspace(id) {
  if (!confirm("删除工作区？其中的会话将移回「未分组」，会话本身不会被删除。")) return;
  try {
    await api(`/api/workspaces/${encodeURIComponent(id)}`, { method: "DELETE" });
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
  loadSessions();
}

// ---- workspace create / rename dialog (dsh browser-owned Modal) ------------
let wsDialogMode = null; // {mode:'create'} | {mode:'rename', id}
function openWsDialog(mode, id, current) {
  wsDialogMode = mode === "rename" ? { mode: "rename", id } : { mode: "create" };
  $("ws-dialog-title").textContent = mode === "rename" ? "重命名工作区" : "新建工作区";
  $("ws-dialog-ok").textContent = mode === "rename" ? "保存" : "创建";
  const inp = $("ws-dialog-input");
  inp.value = current || "";
  const pathInput = $("ws-dialog-path");
  if (pathInput) {
    pathInput.value = "";
    pathInput.classList.toggle("hidden", mode === "rename");
  }
  $("ws-dialog").classList.remove("hidden");
  inp.focus();
  inp.select();
}
function closeWsDialog() {
  $("ws-dialog").classList.add("hidden");
  wsDialogMode = null;
}
async function submitWsDialog() {
  if (!wsDialogMode) return;
  const title = $("ws-dialog-input").value.trim();
  if (!title) return;
  try {
    if (wsDialogMode.mode === "rename") {
      await api(`/api/workspaces/${encodeURIComponent(wsDialogMode.id)}`, {
        method: "PATCH", body: JSON.stringify({ title }),
      });
    } else {
      const path = $("ws-dialog-path")?.value.trim() || "";
      await api("/api/workspaces", { method: "POST", body: JSON.stringify({ title, path }) });
      groupBy = "workspace";
      localStorage.setItem("pa_groupby", "workspace");
    }
    closeWsDialog();
    loadSessions();
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
}
$("ws-dialog-pick").addEventListener("click", async () => {
  try {
    const res = await api("/api/workspaces/pick-directory", { method: "POST" });
    const data = await res.json();
    const pathInput = $("ws-dialog-path");
    if (!pathInput || !data.path) return;
    pathInput.value = data.path;
    const titleInput = $("ws-dialog-input");
    if (titleInput && !titleInput.value.trim()) {
      const normalized = data.path.replace(/[\\/]+$/, "");
      titleInput.value = normalized.split(/[\\/]/).pop() || "";
    }
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
});
$("ws-add").addEventListener("click", () => openWsDialog("create"));
$("ws-dialog-ok").addEventListener("click", submitWsDialog);
$("ws-dialog-cancel").addEventListener("click", closeWsDialog);
$("ws-dialog-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); submitWsDialog(); }
  if (e.key === "Escape") { e.preventDefault(); closeWsDialog(); }
});
$("ws-dialog").addEventListener("click", (e) => {
  if (e.target === $("ws-dialog")) closeWsDialog();
});
// View-options popover: grouped / flat (dsh ViewOptionsMenu).
const viewMenu = $("view-menu");
$("view-toggle").addEventListener("click", (e) => {
  e.stopPropagation();
  viewMenu.classList.toggle("hidden");
});
viewMenu.addEventListener("click", (e) => {
  const v = e.target.dataset.view;
  const o = e.target.dataset.order;
  if (!v && !o) return;
  if (v) { groupBy = v; localStorage.setItem("pa_groupby", v); }
  if (o) { orderBy = o; localStorage.setItem("pa_orderby", o); }
  viewMenu.classList.add("hidden");
  loadSessions();
});
document.addEventListener("click", (e) => {
  if (!e.target.closest("#view-menu, #view-toggle")) viewMenu.classList.add("hidden");
});

let openMenuEl = null;
function closeAnyMenu() {
  if (openMenuEl) { openMenuEl.remove(); openMenuEl = null; }
}
document.addEventListener("click", (e) => { if (!e.target.closest(".si-pop")) closeAnyMenu(); });

function openMenu(li, s) {
  closeAnyMenu();
  hideSessionHover();
  const pop = document.createElement("div");
  pop.className = "si-pop";
  // dsh SessionRowMenu: rename / fork / archive for titled sessions; a blank
  // provisional session has no content to rename/fork/archive, so those verbs
  // stay off — the destructive delete (Shutu AI Agent's local extra, dsh has no
  // delete UI) remains available.
  const items = [];
  if (!s.blank) {
    items.push(`<button data-act="rename">${PA_ICONS.edit}<span>重命名</span></button>`);
    items.push(`<button data-act="fork">${PA_ICONS.branch}<span>派生会话</span></button>`);
    items.push(`<button data-act="archive">${PA_ICONS.archive}<span>归档</span></button>`);
  }
  items.push(`<button data-act="delete" class="danger">${PA_ICONS.trash}<span>删除会话</span></button>`);
  pop.innerHTML = items.join("");
  pop.addEventListener("click", (e) => {
    const act = e.target.closest("button")?.dataset.act;
    if (!act) return;
    closeAnyMenu();
    if (act === "rename") openSessionRename(s);
    if (act === "fork") forkSession(s.id);
    if (act === "archive") archiveSession(s.id);
    if (act === "delete") deleteSession(s.id);
  });
  li.appendChild(pop);
  openMenuEl = pop;
}

// forkSession clones the session (POST /api/sessions/{id}/fork, P6.2) and
// switches to the fresh copy.
async function forkSession(id) {
  try {
    const res = await api(`/api/sessions/${encodeURIComponent(id)}/fork`, { method: "POST" });
    const body = await res.json();
    if (!body.id) throw new Error("no id");
    localStorage.setItem(KEY_CURRENT, body.id);
    currentID = body.id;
    await openSession(body.id);
    loadSessions();
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
}

// archiveSession hides the session from the active tree (P6.2); the log is
// preserved in the store.
async function archiveSession(id) {
  try {
    await api(`/api/sessions/${encodeURIComponent(id)}/archive`, { method: "POST" });
    loadSessions();
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
}

// ---- session rename dialog (dsh browser-owned Modal) ----------------------
// The rename dialog is browser-owned so it outlives row unmounts during a
// sidebar collapse. Rename pins the session's title against automatic revision
// (dsh session-title rename semantics); an unchanged title is allowed.
let srTarget = null; // {id, currentTitle}
function openSessionRename(s) {
  srTarget = { id: s.id, currentTitle: s.title || "" };
  const inp = $("sr-dialog-input");
  inp.value = s.title || "";
  $("sr-dialog").classList.remove("hidden");
  inp.focus();
  inp.select();
}
function closeSessionRename() {
  $("sr-dialog").classList.add("hidden");
  srTarget = null;
}
async function submitSessionRename() {
  if (!srTarget) return;
  const title = $("sr-dialog-input").value.trim();
  if (!title) return;
  try {
    await api(`/api/sessions/${encodeURIComponent(srTarget.id)}/title`, {
      method: "PATCH", body: JSON.stringify({ title }),
    });
    closeSessionRename();
    loadSessions();
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
}
$("sr-dialog-ok").addEventListener("click", submitSessionRename);
$("sr-dialog-cancel").addEventListener("click", closeSessionRename);
$("sr-dialog-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); submitSessionRename(); }
  if (e.key === "Escape") { e.preventDefault(); closeSessionRename(); }
});
$("sr-dialog").addEventListener("click", (e) => {
  if (e.target === $("sr-dialog")) closeSessionRename();
});

async function deleteSession(id) {
  const wasCurrent = id === currentID;
  if (!confirm("确定删除这个会话吗？其所有消息将永久移除。")) return;
  try {
    await api(`/api/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
  if (wasCurrent) {
    if (sseAbort) { sseAbort.abort(); sseAbort = null; }
    streamState = null;
    removeRunning();
    clearDrafts();
    localStorage.removeItem(KEY_CURRENT);
    currentID = "";
    sessionEmpty = true;
    messagesEl.querySelector(".messages-inner")?.remove();
    curSessionEl.textContent = "";
    heroEl.classList.remove("hidden");
    heroWorkspace = "";
    syncHeroChip();
    syncHeroPickState();
    setHeroPhase();
    refreshContextMeter();
    syncGrow();
  }
  loadSessions();
}

// currentGroupKey returns the group owning the active session: the workspace
// id, the UNGROUPED sentinel when the session has no workspace, or "" when
// there is no active session (dsh currentGroup: workspace ?? UNGROUPED_KEY).
const UNGROUPED = "__ungrouped__";
function currentGroupKey() {
  if (!currentID) return "";
  for (const w of wsGroups) if (w.session_ids.includes(currentID)) return w.id;
  const s = lastSessionList.find((x) => x.id === currentID);
  if (s && !s.workspace_id) return UNGROUPED;
  return "";
}
// rememberRecentWorkspace persists the active session's workspace (dsh
// recentWorkspaceId) so 新会话 lands there even without an active session.
function rememberRecentWorkspace(list) {
  lastSessionList = Array.isArray(list) ? list : lastSessionList;
  if (!currentID) return;
  const s = lastSessionList.find((x) => x.id === currentID);
  if (s && s.workspace_id) localStorage.setItem(KEY_RECENT_WS, s.workspace_id);
}
function recentWorkspaceId() {
  return localStorage.getItem(KEY_RECENT_WS) || "";
}
// newSession starts a session in the current group (dsh startSession): the
// target is the current session's workspace — the ungrouped bucket included —
// else the recent workspace, else the hero-picked workspace; with no target
// at all it lands on the choose-workspace hero. The created blank session
// shows as 新会话 in the sidebar.
async function newSession() {
  const group = currentGroupKey();
  const target = group !== "" ? group : (recentWorkspaceId() || heroWorkspace || "");
  if (target === "") { showNewSessionHero(); return; }
  await createSessionInWorkspace(target === UNGROUPED ? "" : target);
}
// showNewSessionHero is the no-workspace fallback of newSession (dsh New
// Session hero): no session is created until the user picks a workspace
// (see pickHeroWorkspace).
function showNewSessionHero() {
  closeDetails();
  currentID = "";
  sessionEmpty = true;
  localStorage.removeItem(KEY_CURRENT);
  if (sseAbort) { sseAbort.abort(); sseAbort = null; }
  if (sseReconnect) { clearTimeout(sseReconnect); sseReconnect = null; }
  streamState = null;
  removeRunning();
  streamActive = false;
  turnRunning = false;
  syncSendButton();
  messagesEl.querySelector(".messages-inner")?.remove();
  curSessionEl.textContent = "";
  sessionCfg = { provider: "", model: "", effort: "", permission: "" };
  heroEl.classList.remove("hidden");
  // Hero composer is inert until a workspace is picked (dsh: choose-workspace
  // placeholder), unless a hero workspace was already chosen previously.
  loadWorkspaces().then(() => { syncHeroChip(); syncHeroPickState(); });
  syncHeroPickState();
  setHeroPhase();
  updatePlaceholder();
  syncHeaderActions();
  refreshContextMeter();
  syncGrow();
}

async function switchSession(id) {
  if (id === currentID) return;
  try {
    await api(`/api/sessions/${encodeURIComponent(id)}/resume`, { method: "POST" });
  } catch (e) { if (e.message !== "unauthorized") console.error(e); return; }
  localStorage.setItem(KEY_CURRENT, id);
  currentID = id;
  closeDetails();
  await openSession(id);
  loadSessions();
}

// ---- new-session hero workspace chip (dsh WorkspaceChip + picker) ----------
// The hero exposes the workspace the next session lands in. Picking one
// materializes the blank session there (dsh connectWorkspace → open), then the
// composer activates. "未分组" keeps the ungrouped option (workspace_id "").
async function loadWorkspaces() {
  try {
    const res = await api("/api/workspaces");
    const data = await res.json();
    wsList = data.workspaces || [];
  } catch (e) { if (e.message !== "unauthorized") console.error(e); wsList = []; }
  return wsList;
}
function heroMenuHTML() {
  const esc = (v) => String(v).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  let html = `<button class="hm-item" role="menuitem" data-ws="" data-label="${esc("未分组")}">
      <span class="hm-ico">${htmlEscapeIcon(PA_ICONS.folderclose16)}</span><span class="hm-label">${esc("未分组")}</span>
    </button>`;
  for (const w of wsList) {
    const sel = heroWorkspace === w.id ? " hm-active" : "";
    const ico = heroWorkspace === w.id ? PA_ICONS.folderopen16 : PA_ICONS.folderclose16;
    html += `<button class="hm-item${sel}" role="menuitem" data-ws="${esc(w.id)}" data-label="${esc(w.title || "工作区")}">
      <span class="hm-ico">${htmlEscapeIcon(ico)}</span><span class="hm-label">${esc(w.title || "工作区")}</span>
    </button>`;
  }
  return html;
}
// icons are pre-escaped static strings (no user content), safe to inject raw.
function htmlEscapeIcon(svg) { return svg || ""; }
function renderHeroMenu() {
  if (!heroWsMenu) return;
  heroWsMenu.innerHTML = heroMenuHTML();
  heroWsMenu.querySelectorAll(".hm-item").forEach((btn) => {
    btn.addEventListener("click", () => pickHeroWorkspace(btn.dataset.ws || "", btn.dataset.label || "未分组"));
  });
}
function toggleHeroMenu(force) {
  const open = force === undefined ? !heroMenuOpen : force;
  heroMenuOpen = open;
  if (heroWsMenu) heroWsMenu.classList.toggle("hidden", !open);
  if (heroWsChip) heroWsChip.setAttribute("aria-expanded", String(open));
}
function syncHeroChip() {
  if (heroWsChip) heroWsChip.setAttribute("aria-expanded", String(heroMenuOpen));
  if (!heroWsLabel) return;
  const w = wsList.find((x) => x.id === heroWorkspace);
  const label = heroWorkspace ? (w && w.title ? w.title : "") : "";
  if (heroWorkspace) {
    heroWsLabel.textContent = label || "工作区";
    heroWsChip?.querySelector(".ws-chip-ico")?.setAttribute("data-icon", "folderopen16");
  } else {
    heroWsLabel.textContent = "选择工作区";
    heroWsChip?.querySelector(".ws-chip-ico")?.setAttribute("data-icon", "folderclose16");
  }
}
function syncHeroPickState() {
  // dsh: no session + no chosen workspace → inert composer (choose-workspace
  // placeholder); a chosen workspace arms the hero composer.
  const inert = !currentID && !heroWorkspace;
  setComposerDisabled(inert);
  updatePlaceholder();
}
async function pickHeroWorkspace(wsId, label) {
  heroWorkspace = wsId;
  toggleHeroMenu(false);
  syncHeroPickState();
  if (currentID) { await openSession(currentID); syncHeroChip(); return; }
  // Materialize the blank session in the chosen workspace (dsh connectWorkspace).
  const created = await createSessionInWorkspace(wsId);
  if (!created) {
    // Roll the pick back so the composer returns to the choose-workspace gate.
    heroWorkspace = "";
    syncHeroChip();
    syncHeroPickState();
  }
}
async function createSessionInWorkspace(wsId) {
  try {
    const body = { workspace_id: wsId };
    if (mode) body.agent_preset = mode;   // Phase 2: lock the staged mode on the new session
    const res = await api("/api/sessions", { method: "POST", body: JSON.stringify(body) });
    const b = await res.json();
    if (!b.id) throw new Error("no id");
    currentID = b.id;
    localStorage.setItem(KEY_CURRENT, b.id);
    await openSession(b.id);
    loadSessions();
    return true;
  } catch (e) {
    if (e.message !== "unauthorized") { console.error(e); toast("创建会话失败"); }
    return false;
  }
}

// ---- hero mode (agent preset) chip + composer toolbar (dsh InputBar) --------
// The mode selector is shutu-agent's mode preset (standard|code|minimal),
// persisted through the settings table (PATCH /api/settings {agent_preset}) and
// re-applied by ApplyModePreset at next launch (the existing semantics: 重启后
// 生效 — no runtime hot-reload of the tool registry). The chip sits beside the
// workspace chip on the hero; the composer toolbar carries command(＋) /
// permission / model / submit below the input box.
function syncModeMenuPosition() {
  if (heroModeMenu && heroModeChip) {
    const r = heroModeChip.getBoundingClientRect();
    heroModeMenu.style.left = r.left + "px";
    heroModeMenu.style.top = (r.bottom + 6) + "px";
    heroModeMenu.style.minWidth = Math.max(r.width, 240) + "px";
  }
}
function renderModeMenu() {
  if (!heroModeMenu) return;
  heroModeMenu.innerHTML = HERO_MODES.map((m) => {
    const sel = mode === m.id ? " hm-active" : "";
    return `<button class="hm-item${sel}" role="menuitem" data-mode="${m.id}">
      <span class="hm-item-text"><span class="hm-item-name">${esc(m.name)}</span><span class="hm-item-desc">${esc(m.desc)}</span></span>
    </button>`;
  }).join("");
  heroModeMenu.querySelectorAll(".hm-item").forEach((btn) => {
    btn.addEventListener("click", () => pickMode(btn.dataset.mode));
  });
}
function toggleModeMenu(force) {
  const open = force === undefined ? !heroModeOpen : force;
  heroModeOpen = open;
  if (heroModeMenu) heroModeMenu.classList.toggle("hidden", !open);
  if (heroModeChip) heroModeChip.setAttribute("aria-expanded", String(open));
}
function syncModeChip() {
  // Hero chip reflects the persisted agent_preset default (stage for new
  // sessions); the topbar mode badge reflects the runtime config.mode.
  const name = MODE_DISPLAY[mode] || (mode ? mode : "");
  if (heroModeLabel) heroModeLabel.textContent = name || "标准模式";
  if (heroModeChip) {
    heroModeChip.setAttribute("aria-expanded", String(heroModeOpen));
    heroModeChip.title = (name || "选择模式") + "（重启后生效）";
  }
}
function syncModeBadge() {
  // dsh AgentPresetLabel: read-only preset/mode name beside the title; the
  // label hides when the runtime mode is unset.
  const rt = config.mode || "";
  modeBadgeEl.textContent = MODE_DISPLAY[rt] || rt;
  modeBadgeEl.classList.toggle("hidden", !modeBadgeEl.textContent);
}
async function pickMode(id) {
  mode = id;
  toggleModeMenu(false);
  syncModeChip();
  try {
    const res = await api("/api/settings", { method: "PATCH", body: JSON.stringify({ agent_preset: id }) });
    if (res.status === 401) return;
    if (!res.ok) throw new Error("HTTP " + res.status);
  } catch (e) {
    if (e.message !== "unauthorized") { console.error("save agent_preset", e); toast("模式保存失败"); }
  }
}

// ---- composer toolbar: command(＋) / permission / model ---------------------
// syncCmdMenuPosition places the composer command menu. The composer sits at
// the bottom edge, so the menu opens upward (same edge logic as the model
// seat); with no room above it flips below the button.
function syncCmdMenuPosition() {
  if (!cmdMenu || !cmdBtn) return;
  const r = cmdBtn.getBoundingClientRect();
  cmdMenu.style.minWidth = Math.max(r.width, 200) + "px";
  const wasHidden = cmdMenu.classList.contains("hidden");
  if (wasHidden) cmdMenu.classList.remove("hidden");
  const h = cmdMenu.offsetHeight || 160;
  if (wasHidden) cmdMenu.classList.add("hidden");
  const GAP = 6, EDGE = 8;
  cmdMenu.style.left = Math.max(EDGE, r.left) + "px";
  const up = r.top - h - GAP;
  cmdMenu.style.top = (up >= EDGE ? up : (r.bottom + GAP)) + "px";
}
function renderCmdMenu() {
  if (!cmdMenu) return;
  const items = [
    { id: "new", label: "新建会话", hint: "回到空白 hero 开始" },
    { id: "archive", label: "归档当前会话", hint: !currentID ? "无会话" : "收起当前会话", disabled: !currentID },
    { id: "delete", label: "删除当前会话", hint: !currentID ? "无会话" : "删除并回到 hero", disabled: !currentID },
  ];
  cmdMenu.innerHTML = items.map((it) =>
    `<button class="hm-item" role="menuitem" data-cmd="${it.id}"${it.disabled ? " disabled" : ""}>
      <span class="hm-item-text"><span class="hm-item-name">${esc(it.label)}</span><span class="hm-item-desc">${esc(it.hint)}</span></span>
    </button>`).join("");
  cmdMenu.querySelectorAll(".hm-item").forEach((btn) => {
    btn.addEventListener("click", () => runCmd(btn.dataset.cmd));
  });
}
function toggleCmdMenu(force) {
  const open = force === undefined ? !cmdMenuOpen : force;
  cmdMenuOpen = open;
  if (cmdMenu) cmdMenu.classList.toggle("hidden", !open);
  if (cmdBtn) cmdBtn.setAttribute("aria-expanded", String(open));
}

// ---- composer slash-command menu (dsh InputTrigger) --------------------------
// The backend accepts a leading slash as a web command. This catalog is loaded
// from the backend config response and gives the textarea dsh-style discovery.
function slashQueryAtCaret() {
  if (!composerText || composerText.selectionStart !== composerText.selectionEnd) return null;
  const caret = composerText.selectionStart;
  const before = composerText.value.slice(0, caret);
  const hit = before.match(/^(\s*)\/([^\s]*)$/);
  if (!hit) return null;
  return { whitespace: hit[1], query: hit[2], caret };
}
function syncSlashMenuPosition() {
  if (!slashMenu || !composerBox) return;
  const r = composerBox.getBoundingClientRect();
  const edge = 8;
  const width = Math.min(320, Math.max(220, r.width));
  slashMenu.style.width = width + "px";
  slashMenu.style.left = Math.max(edge, Math.min(r.left, window.innerWidth - width - edge)) + "px";
  slashMenu.style.top = Math.max(edge, r.top - Math.min(slashMenu.offsetHeight || 240, 320) - 8) + "px";
}
function closeSlashMenu() {
  slashMenuOpen = false;
  slashHighlight = 0;
  if (slashMenu) {
    slashMenu.classList.add("hidden");
    slashMenu.removeAttribute("aria-activedescendant");
  }
}
function renderSlashMenu() {
  if (!slashMenu) return;
  const hit = slashQueryAtCaret();
  if (!hit) { closeSlashMenu(); return; }
  const q = hit.query.toLowerCase();
  const items = webCommands.filter((item) => item.name.startsWith(q));
  if (!items.length) { closeSlashMenu(); return; }
  slashHighlight = Math.min(slashHighlight, items.length - 1);
  slashMenu.innerHTML = items.map((item, index) =>
    (index > 0 && item.kind === "skill" && items[index - 1].kind !== "skill"
      ? '<div class="slash-group-gap" role="separator" aria-hidden="true"></div>' : '') +
    '<button id="slash-option-' + esc(item.name) + '" class="hm-item' + (index === slashHighlight ? ' hm-active' : '') +
    '" role="option" aria-selected="' + (index === slashHighlight) +
    '" data-slash-command="' + esc(item.name) + '">' +
    '<span class="hm-item-text"><span class="hm-item-name">/' + esc(item.name) +
    '</span><span class="hm-item-desc">' + esc(item.hint) + '</span></span></button>'
  ).join("");
  slashMenu.querySelectorAll("[data-slash-command]").forEach((button) => {
    button.addEventListener("mousedown", (e) => {
      e.preventDefault();
      pickSlashCommand(button.dataset.slashCommand || "");
    });
  });
  slashMenuOpen = true;
  slashMenu.classList.remove("hidden");
  slashMenu.setAttribute("aria-activedescendant", "slash-option-" + items[slashHighlight].name);
  syncSlashMenuPosition();
  const active = slashMenu.querySelector(".hm-active");
  if (active && typeof active.scrollIntoView === "function") {
    active.scrollIntoView({ block: "nearest" });
  }
}
function updateSlashMenu() {
  slashHighlight = 0;
  renderSlashMenu();
}
function moveSlashHighlight(direction) {
  const hit = slashQueryAtCaret();
  if (!hit) return false;
  const items = webCommands.filter((item) => item.name.startsWith(hit.query.toLowerCase()));
  if (!items.length) return false;
  slashHighlight = (slashHighlight + direction + items.length) % items.length;
  renderSlashMenu();
  return true;
}
function pickSlashCommand(name) {
  if (!name) return;
  const hit = slashQueryAtCaret();
  if (!hit) return;
  const after = composerText.value.slice(hit.caret);
  composerText.value = hit.whitespace + "/" + name + " " + after;
  const nextCaret = hit.whitespace.length + name.length + 2;
  composerText.setSelectionRange(nextCaret, nextCaret);
  syncGrow();
  updatePlaceholder();
  closeSlashMenu();
  composerText.focus();
}

// ---- composer model seat (dsh ModelSeat) ------------------------------------
// syncModelSeatPosition places the model menu relative to the seat. The
// composer sits at the bottom edge, so the menu opens UPWARD (dsh ModelSelect
// .menu bottom: calc(100% + 8px)); when the space above is too small (hero
// centered composer) it flips below the seat instead.
function syncModelSeatPosition() {
  if (!modelMenu || !modelSeat) return;
  const r = modelSeat.getBoundingClientRect();
  const wasHidden = modelMenu.classList.contains("hidden");
  if (wasHidden) modelMenu.classList.remove("hidden");
  const h = modelMenu.offsetHeight || 260;
  if (wasHidden) modelMenu.classList.add("hidden");
  const GAP = 6, EDGE = 8;
  const left = Math.max(EDGE, Math.min(r.right - 260, window.innerWidth - 260 - EDGE));
  modelMenu.style.left = left + "px";
  const up = r.top - h - GAP;
  if (up >= EDGE) {
    modelMenu.style.top = up + "px";
  } else {
    modelMenu.style.top = (r.bottom + GAP) + "px";
  }
}
// renderModelMenu renders the composer model seat's two-level menu (dsh
// ModelSelect 对齐): the root pane shows the 模型 / 推理等级 rows, each
// drilling into its own list — the provider-grouped model list over the live
// provider directory (the persisted model directory is authoritative for its
// provider: every model the user configured appears, dsh catalog), and the
// effort levels for the target model. Sub-features mirror dsh: the catalog
// refreshes on every open with a loading strip (正在刷新模型列表…), a failed
// refresh shows an error strip with 重新加载, the model list carries the empty
// state 没有可用的模型。, and a selection keeps the menu open (items disabled
// while busy) until the host accepts it.
function renderModelMenu(term) {
  if (!modelMenu) return;
  modelSearch = term || "";
  const q = modelSearch.toLowerCase();
  const activeProv = sessionCfg.provider || config.llm_provider || "";
  const provs = (config.providers || []).filter((p) => p.available || p.id === activeProv);
  const multiple = provs.length > 1;
  const active = sessionCfg.model || config.model || "";
  // The session's own selection wins over the global runtime one (dsh
  // ModelSelection: provider+model+effort are per-session).
  const currentEffort = sessionCfg.effort || config.reasoning_effort || "";
  const reasoningForModel = (prov, model) => {
    const p = provs.find((x) => x.id === prov);
    return (p && p.reasoning && p.reasoning[model]) || null;
  };
  // One provider's selectable models (dsh ModelDirectory: the directory is
  // authoritative for its providers): the current model plus every configured
  // directory entry (llm.profile.<id>.models / custom provider models), else
  // the suggested candidates. Each entry carries its display name and an
  // optional description line (context window for configured models).
  const providerModels = (p) => {
    const out = [];
    const push = (id, name, desc) => { if (!out.some((x) => x.id === id)) out.push({ id, name: name || id, desc: desc || "" }); };
    if (p.model) push(p.model, MODEL_DISPLAY[p.model] || p.model, "");
    const dir = (p.models && p.models.length) ? p.models : null;
    if (dir) {
      for (const m of dir) {
        const id = (m && (m.id || m)) || "";
        if (!id) continue;
        push(id, MODEL_DISPLAY[id] || m.name || id, (m.context_window > 0) ? fmtTokens(m.context_window) + " 上下文" : "");
      }
    } else {
      for (const c of (p.candidates || [])) push(c, MODEL_DISPLAY[c] || c, "");
    }
    return out;
  };
  // Display name of the current model inside a provider (dsh model.name).
  const nameFor = (prov, model) => modelDisplayName(prov, model);
  const effLabel = (eff) => {
    if (eff === "") return "Default"; // dsh effort.providerDefault
    return eff.charAt(0).toUpperCase() + eff.slice(1);
  };
  const effortLabelFor = (prov, model) => {
    const r = reasoningForModel(prov, model);
    if (!r) return undefined;
    // dsh: the effective effort is the explicit selection ?? the model's
    // default effort (deepseek defines high → the trigger shows High, not
    // Default).
    const eff = currentEffort || r.default_effort || "";
    return eff === "" ? "Default" : effLabel(eff);
  };

  const item = (label, sub, opts) =>
    `<button class="hm-item${opts && (opts.active || opts.checked) ? " hm-active" : ""}" role="menuitem" data-action="${opts && opts.action || ""}" data-prov="${opts && opts.prov || ""}" data-model="${opts && opts.model || ""}" data-effort="${opts && opts.effort || ""}"${opts && opts.disabled ? " disabled" : ""}>
      <span class="hm-item-text"><span class="hm-item-name">${esc(label)}</span>${sub ? `<span class="hm-item-desc">${esc(sub)}</span>` : ""}</span>
      ${opts && opts.checked ? `<span class="hm-check">✓</span>` : ""}
      ${opts && opts.chevron ? `<span class="hm-caret">›</span>` : ""}
    </button>`;

  let body = "";
  if (modelPane === "model") {
    // dsh ModelSelect sub-features: loading strip while the catalog refreshes,
    // an error strip with 重新加载 after a failed refresh, provider groups
    // (name + description per model), and the empty state.
    if (catalogLoading) body += `<div class="ms-status">正在刷新模型列表…</div>`;
    if (catalogError) body += `<div class="ms-error"><span>模型操作失败：${esc(catalogError)}</span><button type="button" class="ms-retry">重新加载</button></div>`;
    let shown = 0;
    body += provs.map((g) => {
      const models = providerModels(g).filter((x) => !q || x.id.toLowerCase().includes(q));
      shown += models.length;
      return (multiple && models.length ? `<div class="ms-group">${esc(g.name)}</div>` : "") +
        models.map((m) => {
          const sel = m.id === active && g.id === activeProv;
          const r = reasoningForModel(g.id, m.id);
          return item(m.name, m.desc, {
            action: "pick-model", prov: g.id, model: m.id, checked: sel,
            chevron: !!r, disabled: modelBusy,
          });
        }).join("");
    }).join("");
    if (!shown && !catalogLoading) {
      body += `<div class="ms-empty">没有可用的模型。</div>`;
    }
  } else if (modelPane === "effort") {
    const prov = effortTargetProv;
    const model = effortTarget;
    const r = reasoningForModel(prov, model);
    if (r) {
      // dsh: the provider-default option exists only when the model declares
      // no default effort (deepseek declares high → the listed levels only).
      const effs = r.default_effort ? r.efforts : [{ id: "", name: "Default" }, ...r.efforts];
      const cur = currentEffort || (r.default_effort || "");
      body = effs.map((e) => {
        return item(e.name, "", {
          action: "pick-effort", prov, model, effort: e.id, checked: (e.id || "") === cur,
          disabled: modelBusy,
        });
      }).join("");
    } else {
      body = `<div class="ms-empty">当前模型未提供推理等级。</div>`;
    }
  } else {
    // root pane (dsh ModelSelect root): 模型 / 推理等级 rows. The effort row
    // exists only when the current model offers reasoning (dsh condition).
    const reasoning = reasoningForModel(activeProv, active);
    body = item("模型", nameFor(activeProv, active) || "选择模型", { action: "open-model", chevron: true });
    if (reasoning) body += item("推理等级", effortLabelFor(activeProv, active) || "Default", { action: "open-effort", chevron: true });
  }
  const search = modelPane === "model"
    ? `<div class="ms-search"><input id="model-search" type="text" placeholder="搜索模型…" autocomplete="off" value="${esc(modelSearch)}"></div>`
    : "";
  modelMenu.innerHTML = search + body;
  modelMenu.querySelectorAll(".hm-item").forEach((btn) => {
    if (btn.disabled) return;
    btn.addEventListener("click", async (ev) => {
      ev.stopPropagation();
      const act = btn.dataset.action;
      if (act === "open-model") { modelPane = "model"; renderModelMenu(""); syncModelSeatPosition(); return; }
      if (act === "open-effort") {
        effortTarget = active; effortTargetProv = activeProv;
        modelPane = "effort"; renderModelMenu(""); syncModelSeatPosition(); return;
      }
      if (act === "pick-model") {
        const m = btn.dataset.model, prov = btn.dataset.prov;
        // dsh choose(): selecting the current model just closes.
        if (m === active && prov === activeProv) { modelPane = "root"; toggleModelMenu(false); return; }
        const ok = await setModel(prov, m);
        // dsh settleSelection: close only when the host accepted the selection.
        if (ok) { modelPane = "root"; toggleModelMenu(false); }
        return;
      }
      if (act === "pick-effort") {
        const ok = await setEffort(btn.dataset.effort);
        if (ok) { modelPane = "root"; toggleModelMenu(false); }
        return;
      }
    });
  });
  const retry = modelMenu.querySelector(".ms-retry");
  if (retry) retry.addEventListener("click", () => { catalogError = null; void refreshModelCatalog(); });
  const si = $("model-search");
  if (si) {
    si.focus();
    si.addEventListener("input", () => renderModelMenu(si.value));
    // dsh Escape: a drilled pane backs out to the root first, then closes.
    si.addEventListener("keydown", (e) => {
      if (e.key === "Escape") { e.stopPropagation(); if (modelPane !== "root") { modelPane = "root"; renderModelMenu(""); syncModelSeatPosition(); } else { toggleModelMenu(false); } return; }
      e.stopPropagation();
    });
  } else if (modelMenuOpen) {
    // A pane switch replaced the focused element (drill-in or Escape back to
    // root): move focus to the first item so the resulting focusout (removed
    // element, relatedTarget null) cannot close the menu (dsh keyboard
    // discipline keeps focus inside the menu across panes).
    const first = modelMenu.querySelector(".hm-item:not([disabled])");
    if (first && !modelMenu.contains(document.activeElement)) first.focus();
  }
}
// refreshModelCatalog re-fetches /api/config while the model menu is open
// (dsh: every menu open reloads the directory). Failure keeps the last good
// list and surfaces the error strip with 重新加载.
async function refreshModelCatalog() {
  catalogLoading = true;
  catalogError = null;
  if (modelMenuOpen) renderModelMenu(modelSearch);
  try {
    const res = await api("/api/config");
    if (res.status === 401) return;
    if (!res.ok) throw new Error("HTTP " + res.status);
    config = await res.json();
    loadConfigLabels();
  } catch (e) {
    if (e.message !== "unauthorized") catalogError = e.message || "网络错误";
  } finally {
    catalogLoading = false;
    if (modelMenuOpen) renderModelMenu(modelSearch);
  }
}
function toggleModelMenu(force) {
  const open = force === undefined ? !modelMenuOpen : force;
  modelMenuOpen = open;
  if (modelMenu) modelMenu.classList.toggle("hidden", !open);
  if (modelSeat) modelSeat.setAttribute("aria-expanded", String(open));
}
function runCmd(cmd) {
  toggleCmdMenu(false);
  if (cmd === "new") { newSession(); return; }
  if (!currentID) return;
  if (cmd === "archive") { archiveSession(currentID); }
  else if (cmd === "delete") { deleteSession(currentID); }
}
// modelDisplayName resolves a provider model's display name (dsh model.name):
// the directory entry's name when configured, else the known-display map,
// else the raw id.
function modelDisplayName(prov, model) {
  if (!model) return "";
  const p = (config.providers || []).find((x) => x.id === prov);
  if (p && p.models && p.models.length) {
    const m = p.models.find((x) => (x && (x.id || x)) === model);
    if (m && m.name) return m.name;
  }
  return MODEL_DISPLAY[model] || model;
}
// Current effective model shown on the model seat label (dsh ModelSeat): the
// session override, else the live global model; the effort caption follows the
// same precedence (dsh trigger: "model · effort", effort = selection ?? the
// model's default, "Default" when none). The label shows the model NAME, not
// the raw id (dsh trigger renders model.name).
function syncModelSeat() {
  if (!modelSeatLabel) return;
  const eff = sessionCfg.model || config.model || "";
  const prov = sessionCfg.provider || config.llm_provider || "";
  const name = modelDisplayName(prov, eff) || "模型";
  const r = ((config.providers || []).find((p) => p.id === prov) || {}).reasoning || {};
  const modelEffort = r[eff];
  if (modelEffort) {
    const cur = sessionCfg.effort || config.reasoning_effort || modelEffort.default_effort || "";
    const effortName = cur === "" ? "Default" : (cur.charAt(0).toUpperCase() + cur.slice(1));
    modelSeatLabel.textContent = name + " · " + effortName;
    return;
  }
  modelSeatLabel.textContent = name;
}
// ContextMeter (dsh ContextMeter 对齐): a small occupancy ring beside the send
// button fed by /api/sessions/{id}/context. Hovering the ring shows the dsh
// tooltip sentence plus the detailed figures (~used / window); clicking opens
// the dsh panel (percent + figures + bar). Renders nothing until a session
// provides both usage and a window (dsh: renders nothing until the provider
// reports pressure and capacity).
const CM_RADIUS = 5.5;                        // dsh ring geometry: 14px viewBox, 2px stroke
const CM_CIRCUMFERENCE = 2 * Math.PI * CM_RADIUS;
let cmOpen = false;                           // click panel state (dsh ContextMeter open)

function refreshContextMeter() {
  if (!contextMeter) return;
  if (!currentID) { contextMeter.textContent = ""; cmOpen = false; return; }
  api(`/api/sessions/${encodeURIComponent(currentID)}/context`).then((res) => {
    if (!res.ok) return;
    return res.json().then((d) => {
      const used = d.used_tokens || 0, win = d.context_window || 0;
      if (!win) { contextMeter.textContent = ""; cmOpen = false; return; }
      // dsh contextOccupancy: integer percent, clamped to 100.
      const percent = Math.min(100, Math.round(used / win * 100));
      const figures = "~" + fmtTokens(used) + " / " + fmtTokens(win);
      const arc = (CM_CIRCUMFERENCE * percent / 100).toFixed(2);
      cmOpen = false; // a rebuild closes the panel (fresh content)
      contextMeter.innerHTML =
        `<button type="button" class="cm-trigger" aria-label="上下文已用 ${percent}%" aria-haspopup="dialog" aria-expanded="false">
           <svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true">
             <circle class="cm-track" cx="7" cy="7" r="${CM_RADIUS}"></circle>
             <circle class="cm-fill" cx="7" cy="7" r="${CM_RADIUS}" stroke-dasharray="${arc} ${CM_CIRCUMFERENCE.toFixed(2)}" transform="rotate(-90 7 7)"></circle>
           </svg>
         </button>
         <div class="cm-tip" role="tooltip">上下文已用 <b>${percent}%</b> · ${figures}</div>
         <div class="cm-panel hidden" role="dialog" aria-label="上下文已用">
           <div class="cm-header">
             <span class="cm-headline">上下文已用</span>
             <span class="cm-percent">${percent}%</span>
             <span class="cm-figures">${figures}</span>
           </div>
           <div class="cm-bar"><div class="cm-segment" style="width:${percent}%"></div></div>
         </div>`;
      contextMeter.querySelector(".cm-trigger").addEventListener("click", (e) => {
        e.stopPropagation();
        toggleCMPanel();
      });
    });
  }).catch(() => {});
}
// toggleCMPanel shows/hides the click-open breakdown panel (dsh ContextMeter);
// the tooltip is CSS-hover driven and suppresses itself while the panel is up.
function toggleCMPanel(force) {
  const open = force === undefined ? !cmOpen : force;
  cmOpen = open;
  const panel = contextMeter && contextMeter.querySelector(".cm-panel");
  if (panel) panel.classList.toggle("hidden", !open);
  const trig = contextMeter && contextMeter.querySelector(".cm-trigger");
  if (trig) trig.setAttribute("aria-expanded", String(open));
  if (contextMeter) contextMeter.classList.toggle("cm-open", open);
}
// fmtTokens renders a compact token count (dsh formatTokens): 517 / 12.2K /
// 517K / 1.2M — one decimal only under three digits.
function fmtTokens(n) {
  const scaled = (v) => v >= 100 ? String(Math.round(v)) : String(Math.round(v * 10) / 10);
  if (n < 1000) return String(n);
  if (n < 1000000) return scaled(n / 1000) + "K";
  return scaled(n / 1000000) + "M";
}
// The send button doubles as the run stop (dsh InputBar primaryStops): while a
// turn is in flight it shows the white stop square (dsh ic_stop: 16px rounded
// rect) on the SAME blue circle; otherwise the dsh send arrow (ic_send_outline_14).
const SEND_GLYPH = '<svg viewBox="0 0 14 14" width="14" height="14" aria-hidden="true"><path d="M7.24707 1.01771C7.52897 1.07653 7.77619 1.19694 8.00391 1.38001C8.19202 1.53136 8.39884 1.73784 8.61914 1.95814L12.6396 5.9806L11.6299 6.99134L7.71484 3.0763V13.0001H6.28516V3.0763L2.36914 6.99134L1.35938 5.9806L5.38086 1.95814C5.60116 1.73784 5.80798 1.53136 5.99609 1.38001C6.19476 1.22027 6.4385 1.06739 6.75195 1.01771C6.91296 0.992304 7.07471 0.997504 7.24707 1.01771Z" fill="currentColor"/></svg>';
const STOP_GLYPH = '<svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true"><rect x="3" y="3" width="10" height="10" rx="3" fill="currentColor"></rect></svg>';
function syncSendButton() {
  if (!sendBtn) return;
  sendBtn.classList.toggle("running", turnRunning);
  sendBtn.innerHTML = turnRunning ? STOP_GLYPH : SEND_GLYPH;
  sendBtn.title = turnRunning ? "停止" : "发送";
  sendBtn.setAttribute("aria-label", turnRunning ? "停止" : "发送");
}
let stopRequested = false; // the user pressed STOP; the turn's error is expected
async function stopTurn() {
  if (!currentID) return;
  stopRequested = true;
  try {
    await api(`/api/sessions/${encodeURIComponent(currentID)}/stop`, { method: "POST" });
  } catch (e) { if (e.message !== "unauthorized") console.error("stop", e); }
}
// ---- composer permission seat (dsh PermissionSelect / Access chip) ---------
// The permission tiers map to dsh's shield glyphs: read-only = check,
// workspace-write (standard) = pencil, danger-full-access (full) = exclamation.
// Full access requires an explicit confirmation before it applies (dsh
// RiskConfirmation), so a stray tap can never open the whole whitelist.
const PERMISSIONS = [
  { id: "readonly", label: "只读", desc: "仅允许只读工具（dsh Read-only）" },
  { id: "standard", label: "标准", desc: "标准工具权限（dsh Workspace write）" },
  { id: "full", label: "完全访问", desc: "允许所有工具（dsh Full access）" },
];
const PERMISSION_GLYPHS = {
  readonly: "perm-shield-check",
  standard: "perm-shield-pencil",
  full: "perm-shield-alert",
};
function currentPerm() {
  return sessionCfg.permission || permissionPreset || localStorage.getItem("pa_permission_preset") || "standard";
}
function syncPermSelect() {
  if (!permSeatLabel || !permSeatIcon) return;
  const v = currentPerm();
  const p = PERMISSIONS.find((x) => x.id === v) || PERMISSIONS[1];
  permSeatLabel.textContent = p.label;
  const glyph = PERMISSION_GLYPHS[p.id];
  permSeatIcon.innerHTML = glyph ? htmlEscapeIcon(PA_ICONS[glyph]) : "";
  if (permSeat) {
    permSeat.title = "权限：" + p.desc;
    permSeat.setAttribute("aria-label", "权限：" + p.label);
  }
}
function syncPermSeatPosition() {
  if (!permMenu || !permSeat) return;
  const r = permSeat.getBoundingClientRect();
  const wasHidden = permMenu.classList.contains("hidden");
  if (wasHidden) permMenu.classList.remove("hidden");
  const h = permMenu.offsetHeight || 132;
  if (wasHidden) permMenu.classList.add("hidden");
  const GAP = 6, EDGE = 8;
  permMenu.style.left = Math.max(EDGE, r.left) + "px";
  const up = r.top - h - GAP;
  permMenu.style.top = (up >= EDGE ? up : (r.bottom + GAP)) + "px";
}
function renderPermMenu() {
  if (!permMenu) return;
  const v = currentPerm();
  permMenu.innerHTML = PERMISSIONS.map((p) =>
    `<button class="hm-item${p.id === v ? " hm-active" : ""}" role="menuitemradio" data-perm="${esc(p.id)}">
      <span class="hm-item-ico" aria-hidden="true">${htmlEscapeIcon(PA_ICONS[PERMISSION_GLYPHS[p.id]] || "")}</span>
      <span class="hm-item-text"><span class="hm-item-name">${esc(p.label)}</span><span class="hm-item-desc">${esc(p.desc)}</span></span>
      ${p.id === v ? `<span class="hm-check">✓</span>` : ""}
    </button>`).join("");
  permMenu.querySelectorAll(".hm-item").forEach((btn) => {
    btn.addEventListener("click", (ev) => {
      ev.stopPropagation();
      const id = btn.dataset.perm;
      if (id === v) { togglePermMenu(false); return; }
      if (id === "full") { openFullAccessConfirm(); return; }
      togglePermMenu(false);
      savePermissionPreset(id);
    });
  });
}
function togglePermMenu(force) {
  const open = force === undefined ? !permMenuOpen : force;
  permMenuOpen = open;
  if (permMenu) permMenu.classList.toggle("hidden", !open);
  if (permSeat) permSeat.setAttribute("aria-expanded", String(open));
}
// Full access requires explicit acknowledgement (dsh RiskConfirmation): the
// modal is a small overlay asking the user to check the risk box before the
// tier applies.
function openFullAccessConfirm() {
  const overlay = document.createElement("div");
  overlay.className = "perm-confirm-overlay";
  overlay.innerHTML = `<div class="perm-confirm-modal" role="alertdialog" aria-modal="true">
    <div class="perm-confirm-title">启用完全访问</div>
    <div class="perm-confirm-desc">完全访问允许智能体执行所有已注册工具（包括写入和修改操作）。请确认你了解这一风险。</div>
    <label class="perm-confirm-check"><input type="checkbox" id="perm-confirm-box"> 我了解完全访问的风险</label>
    <div class="perm-confirm-actions">
      <button type="button" class="m-btn m-secondary" id="perm-confirm-cancel">取消</button>
      <button type="button" class="m-btn m-primary" id="perm-confirm-ok" disabled>启用</button>
    </div>
  </div>`;
  document.body.appendChild(overlay);
  const close = () => overlay.remove();
  overlay.addEventListener("click", (e) => { if (e.target === overlay) close(); });
  overlay.querySelector("#perm-confirm-cancel").addEventListener("click", close);
  overlay.querySelector("#perm-confirm-box").addEventListener("change", (e) => {
    overlay.querySelector("#perm-confirm-ok").disabled = !e.target.checked;
  });
  overlay.querySelector("#perm-confirm-ok").addEventListener("click", () => {
    close();
    togglePermMenu(false);
    savePermissionPreset("full");
  });
}
async function savePermissionPreset(v) {
  permissionPreset = v;
  localStorage.setItem("pa_permission_preset", v);
  if (currentID) {
    // Per-session tier: update the active session's override (mode is locked).
    // The PATCH rewrites the whole selection, so the current provider/model/
    // effort ride along (dsh ModelSelection).
    try {
      const res = await api(`/api/sessions/${encodeURIComponent(currentID)}/config`, {
        method: "PATCH", body: JSON.stringify({
          provider: sessionCfg.provider || "",
          model: sessionCfg.model || "",
          reasoning_effort: sessionCfg.effort || "",
          permission: v,
        }),
      });
      if (res.status === 401) return;
      if (!res.ok) throw new Error("HTTP " + res.status);
      sessionCfg.permission = v;
    } catch (e) {
      if (e.message !== "unauthorized") { console.error("session permission", e); toast("权限保存失败"); }
    }
    syncPermSelect();
    return;
  }
  // No active session: persist the global default tier (applied at next launch).
  try {
    const res = await api("/api/settings", { method: "PATCH", body: JSON.stringify({ permission_preset: v }) });
    if (res.status === 401) return;
    if (!res.ok) throw new Error("HTTP " + res.status);
  } catch (e) {
    if (e.message !== "unauthorized") { console.error("save permission_preset", e); toast("权限保存失败"); }
  }
  syncPermSelect();
}
async function setModel(provider, model) {
  if (!provider) { syncModelSeat(); return true; }
  modelBusy = true;
  if (modelMenuOpen) renderModelMenu(modelSearch);
  try {
    if (currentID) {
      // Per-session selection (dsh ModelSelection): provider+model+effort are
      // one selection; PATCH rewrites all of them, so the full current
      // selection is sent every time.
      const res = await api(`/api/sessions/${encodeURIComponent(currentID)}/config`, {
        method: "PATCH", body: JSON.stringify({
          provider, model,
          reasoning_effort: sessionCfg.effort || "",
          permission: sessionCfg.permission,
        }),
      });
      if (res.status === 401) return false;
      if (!res.ok) { toast("模型切换失败"); return false; }
      sessionCfg.provider = provider;
      sessionCfg.model = model;
      syncModelSeat();
      return true;
    }
    // No active session: live global model switch.
    const res = await api("/api/config/model", { method: "POST", body: JSON.stringify({ provider, model }) });
    if (res.status === 401) return false;
    if (!res.ok) { toast("模型切换失败"); await loadConfig(); return false; }
    await loadConfig();
    return true;
  } catch (e) {
    if (e.message !== "unauthorized") { console.error("session model", e); toast("模型切换失败"); }
    return false;
  } finally {
    modelBusy = false;
    if (modelMenuOpen) renderModelMenu(modelSearch);
  }
}

// setEffort applies a thinking-effort selection (dsh ModelSelect 推理等级):
// "" restores the provider default; "off"|"low"|"high"|"max" select a level.
// With an active session the effort rides the per-session selection (PATCH);
// on the hero it is runtime-only, like the live model switch (POST
// /api/config/model).
async function setEffort(effort) {
  modelBusy = true;
  if (modelMenuOpen) renderModelMenu(modelSearch);
  try {
    if (currentID) {
      const res = await api(`/api/sessions/${encodeURIComponent(currentID)}/config`, {
        method: "PATCH", body: JSON.stringify({
          provider: sessionCfg.provider || "",
          model: sessionCfg.model || "",
          reasoning_effort: effort || "",
          permission: sessionCfg.permission,
        }),
      });
      if (res.status === 401) return false;
      if (!res.ok) { toast("推理等级切换失败"); return false; }
      sessionCfg.effort = effort || "";
      syncModelSeat();
      return true;
    }
    const res = await api("/api/config/model", {
      method: "POST", body: JSON.stringify({ reasoning_effort: effort || "" }),
    });
    if (res.status === 401) return false;
    if (!res.ok) { toast("推理等级切换失败"); await loadConfig(); return false; }
    await loadConfig();
    syncModelSeat();
    return true;
  } catch (e) {
    if (e.message !== "unauthorized") { console.error("switch effort", e); toast("推理等级切换失败"); }
    return false;
  } finally {
    modelBusy = false;
    if (modelMenuOpen) renderModelMenu(modelSearch);
  }
}
// Load the active session's per-session overrides (Phase 2) and bind the
// composer permission + model pickers. An empty id (hero) resets to globals.
async function loadSessionConfig(id) {
  sessionCfg = { provider: "", model: "", effort: "", permission: "" };
  if (!id) { syncPermSelect(); syncModelSeat(); return; }
  try {
    const res = await api(`/api/sessions/${encodeURIComponent(id)}/config`);
    if (res.status === 401) return;
    const d = await res.json();
    sessionCfg = {
      provider: d.provider || "", model: d.model || "",
      effort: d.reasoning_effort || "", permission: d.permission || "",
    };
  } catch (e) {
    if (e.message !== "unauthorized") { console.error("session config", e); sessionCfg = { provider: "", model: "", effort: "", permission: "" }; }
  }
  syncPermSelect();
  syncModelSeat();
}

async function loadFeedback(id) {
  if (!id || currentID !== id) return;
  feedbackBySeq = new Map();
  try {
    const res = await api(`/api/sessions/${encodeURIComponent(id)}/feedback`);
    if (res.status === 401) return;
    if (!res.ok) throw new Error("HTTP " + res.status);
    const items = await res.json();
    if (currentID !== id) return;
    for (const item of items) {
      if (item && Number.isSafeInteger(item.seq) && (item.rating === "positive" || item.rating === "negative")) {
        feedbackBySeq.set(item.seq, item.rating);
      }
    }
  } catch (e) {
    if (e.message !== "unauthorized") console.error("session feedback", e);
  }
}
// Composer pref loading: the mode comes from the config view (loadConfigLabels);
// the permission preset is a persisted setting (fallback localStorage).
async function loadComposerPrefs() {
  try {
    const res = await api("/api/settings");
    if (res.status === 401) return;
    const d = await res.json();
    if (d.permission_preset) { permissionPreset = d.permission_preset; syncPermSelect(); }
    else { permissionPreset = localStorage.getItem("pa_permission_preset") || "standard"; syncPermSelect(); }
    if (d.agent_preset) { mode = d.agent_preset; syncModeChip(); }
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
}

// ---- right details panel (dsh ui-conversation DetailsPanel) ----------------
// Shows the selected tool call's 输入 (args) and 输出 (result); an empty hint
// states how to select one. Opening selects; the close button collapses the
// column back to 0 width (mount kept, column collapses — dsh concession).
function prettyJson(raw) {
  try { return JSON.stringify(JSON.parse(raw), null, 2); } catch (_) { return raw; }
}
function escCode(s) {
  return String(s ?? "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}
function openDetails(seq) {
  const meta = toolMeta[seq];
  if (!meta) return;
  selectedTool = { seq, name: meta.name, args: meta.args || "", output: meta.output || "", error: !!meta.error };
  details.open = true;
  if (detailsPanel) detailsPanel.classList.remove("hidden");
  renderDetails();
  markSelectedTool();
  renderColumns();
}
function closeDetails() {
  selectedTool = null;
  details.open = false;
  if (detailsPanel) detailsPanel.classList.add("hidden");
  markSelectedTool();
  renderColumns();
}
function renderDetails() {
  if (!detailsTitle) return;
  if (selectedTool) {
    detailsTitle.textContent = selectedTool.name;
    detailsEmptyEl.classList.add("hidden");
    detailsSelEl.classList.remove("hidden");
    let html = "";
    if (selectedTool.args) {
      html += `<section class="details-sec"><div class="details-sec-label">输入</div><div class="details-codewrap"><button type="button" class="details-copy" data-copy="1">复制</button><pre class="details-pre">${escCode(prettyJson(selectedTool.args))}</pre></div></section>`;
    }
    html += `<section class="details-sec"><div class="details-sec-label">输出</div><div class="details-codewrap"><button type="button" class="details-copy" data-copy="1">复制</button><pre class="details-pre${selectedTool.error ? " details-err" : ""}">${escCode(selectedTool.output)}</pre></div></section>`;
    detailsSelEl.innerHTML = html;
    detailsSelEl.querySelectorAll(".details-copy").forEach((btn) => {
      btn.addEventListener("click", () => {
        const text = btn.parentElement.querySelector(".details-pre")?.textContent || "";
        if (!text) return;
        (navigator.clipboard && navigator.clipboard.writeText)
          ? navigator.clipboard.writeText(text)
          : (btn.textContent = "已复制");
        if (navigator.clipboard) btn.textContent = "已复制";
        setTimeout(() => { btn.textContent = "复制"; }, 1200);
      });
    });
  } else {
    detailsTitle.textContent = "详情";
    detailsEmptyEl.classList.remove("hidden");
    detailsSelEl.classList.add("hidden");
  }
}
function markSelectedTool() {
  document.querySelectorAll(".msg.tool[data-seq]").forEach((n) => {
    n.classList.toggle("selected", !!selectedTool && n.dataset.seq === selectedTool.seq);
  });
}
function syncHeroMenuPosition() {
  if (heroWsMenu && heroWsChip) {
    const r = heroWsChip.getBoundingClientRect();
    heroWsMenu.style.left = r.left + "px";
    heroWsMenu.style.top = (r.bottom + 6) + "px";
    heroWsMenu.style.minWidth = Math.max(r.width, 220) + "px";
  }
}

// ---- session view: messages + SSE ------------------------------------------
// setHeroPhase drives the center column's phase (dsh ConversationRoot
// data-phase): hero → the composer centers under the headline/workspace/mode
// row; active → the transcript scrolls and the composer docks at the bottom.
// The hero phase holds for a BLANK session too (create → first message), not
// only when there is no session, so the composer moves down only after the
// first submit. The topbar status row follows the same rule (dsh
// ConversationSessionHeader headerHidden: a blank session shows no
// title/mode/runs chrome; only a session with content gets the header).
function setHeroPhase() {
  heroActive = !currentID || sessionEmpty;
  if (colCenterEl) colCenterEl.dataset.phase = heroActive ? "hero" : "active";
  heroEl.classList.toggle("hidden", !heroActive);
  if (topbarEl) topbarEl.classList.toggle("hidden", heroActive);
}
// syncSessionTitle fills the topbar title with the session's derived title
// (dsh breadcrumb displayTitle); falls back to the raw id while the list is
// stale.
function syncSessionTitle() {
  if (!currentID) { curSessionEl.textContent = ""; return; }
  const s = lastSessionList.find((x) => x.id === currentID);
  curSessionEl.textContent = (s && s.title) ? s.title : currentID;
}

function openSession(id) {
  if (sseAbort) { sseAbort.abort(); sseAbort = null; }
  if (sseReconnect) { clearTimeout(sseReconnect); sseReconnect = null; }
  streamState = null;
  removeRunning();
  streamActive = false;
  turnRunning = false;
  renderedSeqs = new Set(); // per-session rendered-event dedup
  feedbackBySeq = new Map();
  resetCompactionRows();
  syncSendButton();
  messagesEl.querySelector(".messages-inner")?.remove();
  syncSessionTitle();
  toolMeta = {};
  sessionEmpty = !id;
  heroEl.classList.toggle("hidden", !!id);
  // A real session re-enables the composer; the hero phase keeps it gated on a
  // picked workspace. (dsh: session → composer active, hero → choose-workspace.)
  setComposerDisabled(!id);
  updatePlaceholder();
  syncHeaderActions();
  if (!id) { sessionCfg = { model: "", permission: "" }; setHeroPhase(); if (contextMeter) { contextMeter.textContent = ""; cmOpen = false; } return; }
  loadSessionConfig(id);
  return loadFeedback(id).then(() => Promise.all([loadEvents(id), connectStream(id)]));
}

async function loadEvents(id) {
  try {
    // openSession reset the rendered set before calling this, so any growth
    // during the fetch means the SSE stream rendered events concurrently — a
    // live turn (the user sent a message right after opening) or the replay.
    // In that case the snapshot is stale by definition: wiping the container
    // would destroy the in-flight round's already-rendered output and the
    // stale set rebuild would lose its seqs forever. The stream owns the
    // container then — render nothing, merge nothing.
    const grewFrom = renderedSeqs.size;
    const res = await api(`/api/sessions/${encodeURIComponent(id)}/events`);
    const evs = await res.json();
    if (renderedSeqs.size > grewFrom) {
      sessionEmpty = evs.length === 0;
      setHeroPhase();
      if (!sessionEmpty) heroEl.classList.add("hidden");
      refreshContextMeter();
      return;
    }
    const inner = msgInner();
    inner.textContent = "";
    let lastTime = "";
    for (const ev of evs) {
      lastTime = ev.time || lastTime;
      renderEvent(ev, true);
    }
    // A session with no events stays on the hero (centered composer); once it
    // has history the phase flips to active and the composer docks.
    sessionEmpty = evs.length === 0;
    setHeroPhase();
    if (!sessionEmpty) heroEl.classList.add("hidden");
    // The SSE stream (re)connect replays the same stored events; everything
    // rendered here is deduped from the stream by rendered seq.
    renderedSeqs = new Set();
    for (const ev of evs) if (ev.seq != null) renderedSeqs.add(ev.seq);
    refreshContextMeter();
    scrollToBottom(true);
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
}

async function downloadSessionExport() {
  if (!currentID) return;
  try {
    const res = await api(`/api/session.export?sessionId=${encodeURIComponent(currentID)}&includeDescendants=true`);
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      throw new Error(body.error || ("HTTP " + res.status));
    }
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `shutu-session-${currentID.replace(/[^A-Za-z0-9_-]/g, "_")}.zip`;
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  } catch (e) {
    if (e.message !== "unauthorized") addErrorRow({ summary: "Session export failed: " + e.message });
  }
}

function renderEvent(ev, replay) {
  // First event of an empty session: the turn has begun, so leave the centered
  // hero and dock the composer (dsh: 第一次输入提交后输入条下移).
  if (sessionEmpty) { sessionEmpty = false; setHeroPhase(); }
  switch (ev.type) {
    case "user/message": {
      if (ev.compaction_marker) {
        addCompactionEvent(ev);
        break;
      }
      if (queueSlashCommand(ev)) break;
      const imgs = (ev.images || []).map((iv) => ({
        src: `/api/sessions/${encodeURIComponent(currentID)}/attachments/${iv.id}`,
        id: iv.id,
      }));
      addUserMsg(ev.summary || "", ev.time, imgs.length ? imgs : null);
      reasoningLive = false; // a new turn starts its own thinking card
      break;
    }
    case "assistant/reasoning":
      // Streamed thinking delta: accumulate into the in-place Think row so it
      // sits above the step's tool calls (dsh order: 思考 → 工具调用 → 文本).
      reasoningLive = true;
      addReasoning(ev.reasoning || "", ev.time, ev.seq);
      break;
    case "assistant/message":
      // The joined reasoning already streamed as assistant/reasoning deltas;
      // only legacy logs (reasoning without deltas) add the card here.
      {
        const command = takeSlashCommand();
        if (command) {
          // A successful /compact is already represented by its dedicated dsh
          // compaction row. Failed/no-history compactions have no lifecycle row
          // and retain the generic command card, matching dsh's fallback.
          if (command.name === "compact" && (pendingCompactionRow || compactionRows.size)) {
            streamState = null;
            break;
          }
          streamState = null;
          addCommandRow({ command: command.name, summary: ev.summary, seq: ev.seq });
          break;
        }
      }
      if (ev.reasoning && !reasoningLive) addReasoning(ev.reasoning, ev.time);
      if (ev.reasoning || reasoningLive) settleReasoning();
      reasoningLive = false;
      finishAssistant(ev.summary || "", ev.time, ev.seq);
      break;
    case "web/command-result":
      addCommandRow({ command: ev.command || "command", summary: ev.summary, seq: ev.seq });
      if (!replay && ev.command === "export") void downloadSessionExport();
      break;
    case "compaction/start":
      addCompactionEvent(ev);
      break;
    case "tool/start":
    case "tool/result":
    case "tool/error":
      if (ev.type === "tool/error" && !ev.tool_name) addErrorRow(ev);
      else renderToolRow(ev);
      break;
    case "kb/recall":
    case "skill/catalog":
    case "compaction/summary":
    case "compaction/end":
      addCompactionEvent(ev);
      if (!replay && ev.type === "compaction/end") refreshContextMeter();
      break;
    default: break;
  }
}

// connectStream: fetch-based SSE (token stays in the Authorization header;
// EventSource cannot set it — ADR D-WEB2-B).
async function connectStream(id) {
  sseAbort = new AbortController();
  const ac = sseAbort;
  let buf = "";
  const tryConnect = async () => {
    if (ac.signal.aborted) return;
    try {
      const headers = {};
      if (token()) headers.Authorization = "Bearer " + token();
      const res = await fetch(`/api/sessions/${encodeURIComponent(id)}/events/stream`, {
        headers, signal: ac.signal,
      });
      if (res.status === 401) { showLogin("令牌无效或已过期"); return; }
      if (!res.ok || !res.body) return;
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf("\n\n")) !== -1) {
          const frame = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          for (const line of frame.split("\n")) {
            if (line.startsWith("data: ")) {
              try { handleStreamEvent(JSON.parse(line.slice(6))); }
              catch (_) { /* skip malformed frame */ }
            }
          }
        }
      }
    } catch (e) {
      if (ac.signal.aborted) return;
    }
    // stream ended (server closed or network): reconnect after 3s
    if (!ac.signal.aborted && document.visibilityState !== "hidden") {
      sseReconnect = setTimeout(tryConnect, 3000);
    }
  };
  tryConnect();
}

function handleStreamEvent(ev) {
  if (!currentID) return;
  // The stream replays the stored history on every (re)connect; events already
  // rendered (from loadEvents or a previous connection) must not render again —
  // otherwise every reconnect duplicates messages and re-flashes settled rows.
  // The SSE field is lowercase `seq` (eventView json tag).
  if (ev.seq != null) {
    if (renderedSeqs.has(ev.seq)) return;
  }
  if (ev.type === "assistant/chunk") {
    appendAssistantStreaming(ev.summary || "", ev.seq);
    noteRendered(ev.seq);
    return;
  }
  renderEvent(ev, false);
  noteRendered(ev.seq);
  if (ev.type === "assistant/message") { streamState = null; }
}

// reconcileEvents re-fetches the session log and renders only events the SSE
// stream never delivered (the hub drops a slow subscriber's tail events when
// its buffer is full). The rendered-seq SET — not a watermark — lets a dropped
// event (a gap before later-rendered events) still be repaired here.
// assistant/chunk and assistant/reasoning are skipped — the closing
// assistant/message is authoritative for both text and reasoning — and so is
// user/message: the POST settles while the turn's user/message SSE frame may
// still be in flight, and re-rendering it at the END of the transcript would
// show the round's own input as a ghost bubble below the answer. A user
// message dropped by the hub is recovered by the reconnect replay (log order)
// or the next session open. Their seqs are still recorded so a later replay
// skips them.
async function reconcileEvents() {
  try {
    const res = await api(`/api/sessions/${encodeURIComponent(currentID)}/events`);
    if (!res.ok) return;
    const evs = await res.json();
    let advanced = false;
    for (const ev of evs) {
      if (ev.seq != null && renderedSeqs.has(ev.seq)) continue;
      if (ev.type === "assistant/chunk" || ev.type === "assistant/reasoning") {
        noteRendered(ev.seq);
        continue;
      }
      if (ev.type === "user/message") {
        // Reconcile must not append a late ordinary user bubble below its
        // answer, but a dropped slash command still has to be paired with its
        // assistant result so it can become a dsh command row.
        queueSlashCommand(ev);
        noteRendered(ev.seq);
        continue;
      }
      renderEvent(ev, true);
      noteRendered(ev.seq);
      advanced = true;
    }
    if (advanced) scrollToBottom(false);
  } catch (e) { if (e.message !== "unauthorized") console.error("reconcile", e); }
}

// ---- composer ---------------------------------------------------------------
function syncGrow() {
  // The hidden ::after replica layer (grow-wrap) sizes the textarea; it reads
  // the value through the data attribute so only one live source exists.
  growWrapEl.dataset.replicatedValue = composerText.value + "\n";
}
function setComposerDisabled(disabled) {
  if (disabled) closeSlashMenu();
  composerBox.classList.toggle("disabled", disabled);
  // The send seat becomes the STOP control while a turn runs (dsh): it must
  // stay clickable in the running state even though the composer is locked.
  sendBtn.disabled = disabled && !turnRunning;
  composerText.disabled = disabled;
  // The toolbar controls follow the same lock (dsh: the model seat stays live
  // only while a session exists; here the whole toolbar locks while inert).
  [permSeat, modelSeat, cmdBtn].forEach((el) => { if (el) el.disabled = disabled; });
}
function placeholderFor() {
  if (currentID) return "给智能体发消息…";
  if (heroWorkspace) return "描述你想要构建的内容";
  return "选择一个工作区开始";
}
function updatePlaceholder() {
  composerText.placeholder = placeholderFor();
}
composerText.addEventListener("input", () => {
  syncGrow();
  updatePlaceholder();
  updateSlashMenu();
});

async function sendMessage() {
  const text = composerText.value.trim();
  if ((!text && drafts.length === 0) || !currentID) return;
  setComposerDisabled(true);
  try {
    // No optimistic bubble: the backend appends user/message and streams it
    // back over SSE, so an optimistic render would duplicate the message.
    // Submitting the first message moves the composer from the centered hero
    // down to the docked slot (dsh: 第一次输入提交后输入条下移).
    if (sessionEmpty) { sessionEmpty = false; setHeroPhase(); }
    addRunning();
    streamActive = true;
    turnRunning = true;
    syncSendButton();
    sendBtn.disabled = false; // running → the button is the STOP control (dsh)
    loadSessions(); // blue running dot on the current row
    let images = [];
    if (drafts.length) {
      for (const d of drafts) {
        const id = await uploadDraft(d);
        if (!id) throw new Error("图片上传失败，已保留草稿");
        images.push(id);
      }
      clearDrafts();
    }
    composerText.value = "";
    syncGrow();
    closeSlashMenu();
    const res = await api(`/api/sessions/${encodeURIComponent(currentID)}/message`, {
      method: "POST",
      body: JSON.stringify({ text, images }),
    });
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      throw new Error(body.error || ("HTTP " + res.status));
    }
    // /permission persists the active session override on the backend; reload
    // it so the permission seat reflects a typed slash command immediately.
    if (/^\/permission(?:\s|$)/.test(text)) await loadSessionConfig(currentID);
  } catch (e) {
    if (e.message !== "unauthorized") {
      // A user-initiated stop aborts the turn — the POST settles with an
      // error, but that is the expected outcome, not a failed round.
      if (stopRequested) { stopRequested = false; removeRunning(); }
      else {
        removeRunning();
        addErrorRow({ summary: e.message });
        console.error(e);
      }
    }
  } finally {
    setComposerDisabled(false);
    stopRequested = false;
    // The POST settles exactly when the turn settles (success or error): a
    // failed turn produces no assistant/message event, so finishAssistant
    // never ran — reset the run state (including the turn-level Deep diving
    // indicator) and refresh the ContextMeter here.
    if (streamActive) { streamActive = false; turnRunning = false; syncSendButton(); loadSessions(); removeRunning(); }
    // The SSE hub may have dropped tail events (full subscriber buffer), which
    // would leave the last tool row / Think row sweeping forever. Reconcile
    // from the durable log, then park anything still running (cancelled turn).
    reconcileEvents().then(() => {
      settleReasoning();
      const inner = msgInner();
      const running = [...inner.querySelectorAll(".msg.tool[data-state='running']")];
      for (const n of running) {
        const c = n.dataset.call;
        if (c) {
          // An orphaned duplicate (same call settled in another row, e.g. from
          // a pre-fix replay): remove it instead of parking it yellow.
          const escC = String(c).replace(/"/g, "\\\"");
          const settled = inner.querySelector(
            `.msg.tool[data-call="${escC}"][data-state='ok'], .msg.tool[data-call="${escC}"][data-state='error']`);
          if (settled) { n.remove(); continue; }
        }
        n.dataset.state = "stopped";
        const lead = n.querySelector(".dsh-leading");
        if (lead) lead.innerHTML = '<span class="dsh-statedot dsh-statedot-warn"></span>';
      }
    });
    refreshContextMeter();
  }
}

// ---- P5: image attachments (dsh ui-attachment) -------------------------------
const MAX_DRAFTS = 10, MAX_IMG_BYTES = 10 * 1024 * 1024;
const ACCEPTED_TYPES = ["image/png", "image/jpeg", "image/webp", "image/gif"];
let drafts = [];

function draftAcceptable(f) { return ACCEPTED_TYPES.includes(f.type); }
function addDraftFile(f) {
  if (!draftAcceptable(f)) { toast("仅支持 PNG / JPG / WebP / GIF 图片"); return; }
  if (f.size > MAX_IMG_BYTES) { toast("图片超过 10MB"); return; }
  if (drafts.length >= MAX_DRAFTS) { toast("一次最多 10 张图片"); return; }
  drafts.push({ id: (crypto.randomUUID ? crypto.randomUUID() : String(Date.now() + Math.random())), file: f, name: f.name, url: URL.createObjectURL(f) });
  renderDraftRail();
}
function renderDraftRail() {
  let rail = document.querySelector(".draft-rail");
  if (drafts.length === 0) { if (rail) rail.remove(); return; }
  if (!rail) {
    rail = document.createElement("div");
    rail.className = "draft-rail";
    composerBox.closest(".composer-card").before(rail);
  }
  rail.textContent = "";
  for (const d of drafts) {
    const th = document.createElement("div");
    th.className = "draft-thumb";
    th.innerHTML = `<img src="${d.url}" alt="${esc(d.name)}"><button class="draft-remove" title="移除">✕</button>`;
    th.querySelector(".draft-remove").addEventListener("click", (e) => { e.stopPropagation(); removeDraft(d); });
    th.addEventListener("click", () => openLightbox(d.url));
    rail.appendChild(th);
  }
  const count = document.createElement("span");
  count.className = "draft-count";
  count.textContent = `${drafts.length}/${MAX_DRAFTS}`;
  rail.appendChild(count);
}
function removeDraft(d) {
  drafts = drafts.filter((x) => x !== d);
  URL.revokeObjectURL(d.url);
  renderDraftRail();
}
function clearDrafts() {
  for (const d of drafts) URL.revokeObjectURL(d.url);
  drafts = [];
  renderDraftRail();
}
async function uploadDraft(d) {
  const fd = new FormData();
  fd.append("file", d.file, d.name);
  const headers = {};
  if (token()) headers.Authorization = "Bearer " + token();
  const res = await fetch(`/api/sessions/${encodeURIComponent(currentID)}/attachments`, { method: "POST", headers, body: fd });
  if (res.status === 401) { showLogin("令牌无效或已过期"); throw new Error("unauthorized"); }
  if (!res.ok) return null;
  const body = await res.json();
  return body.id || null;
}

// paste + whole-page drop (dsh has no upload button — only these two paths)
composerText.addEventListener("paste", (e) => {
  const files = e.clipboardData ? [...e.clipboardData.files] : [];
  const imgs = files.filter(draftAcceptable);
  if (imgs.length) { e.preventDefault(); for (const f of imgs) addDraftFile(f); }
});
let dragDepth = 0;
function hasImageFiles(dt) {
  return dt && dt.types && dt.types.includes("Files") &&
    [...(dt.items || [])].some((i) => i.kind === "file" && i.type.startsWith("image/"));
}
document.addEventListener("dragover", (e) => {
  if (!hasImageFiles(e.dataTransfer)) return;
  e.preventDefault();
  dragDepth++;
  showDropOverlay();
});
document.addEventListener("dragleave", (e) => {
  dragDepth = Math.max(0, dragDepth - 1);
  if (dragDepth === 0) hideDropOverlay();
});
document.addEventListener("drop", (e) => {
  dragDepth = 0;
  hideDropOverlay();
  if (!hasImageFiles(e.dataTransfer)) return;
  e.preventDefault();
  for (const f of e.dataTransfer.files) addDraftFile(f);
});
function showDropOverlay() {
  let ov = document.querySelector(".drop-overlay");
  if (!ov) { ov = document.createElement("div"); ov.className = "drop-overlay"; ov.textContent = "松开以添加图片"; document.body.appendChild(ov); }
}
function hideDropOverlay() { document.querySelector(".drop-overlay")?.remove(); }

// lightbox (original-size preview)
function openLightbox(src) {
  const lb = document.createElement("div");
  lb.className = "lightbox";
  lb.innerHTML = `<img src="${esc(src)}" alt="原图"><button class="lb-close" title="关闭">✕</button>`;
  const close = () => lb.remove();
  lb.addEventListener("click", (e) => { if (e.target === lb || e.target.classList.contains("lb-close")) close(); });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") close(); }, { once: true });
  document.body.appendChild(lb);
}

// mini toast
let toastTimer = null;
function toast(msg) {
  let t = document.querySelector(".toast");
  if (!t) { t = document.createElement("div"); t.className = "toast"; document.body.appendChild(t); }
  t.textContent = msg;
  t.classList.add("show");
  if (toastTimer) clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove("show"), 2600);
}
composerText.addEventListener("keydown", (e) => {
  if (slashMenuOpen) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      moveSlashHighlight(1);
      return;
    }
    if (e.key === "ArrowUp") {
      e.preventDefault();
      moveSlashHighlight(-1);
      return;
    }
    if ((e.key === "Tab" || e.key === "Enter") && !e.isComposing) {
      const hit = slashQueryAtCaret();
      const items = hit ? webCommands.filter((item) => item.name.startsWith(hit.query.toLowerCase())) : [];
      if (items[slashHighlight]) {
        e.preventDefault();
        pickSlashCommand(items[slashHighlight].name);
        return;
      }
    }
    if (e.key === "Escape") {
      e.preventDefault();
      closeSlashMenu();
      return;
    }
  }
  // Composer send key follows the General-settings preference: "send" sends on
  // plain Enter (Shift+Enter newline); "newline" sends on Ctrl/Cmd+Enter only.
  const mode = localStorage.getItem("pa_enter") || "send";
  const isSend = mode === "send"
    ? (e.key === "Enter" && !e.shiftKey && !e.isComposing)
    : (e.key === "Enter" && (e.ctrlKey || e.metaKey) && !e.isComposing);
  if (isSend) {
    e.preventDefault();
    sendMessage();
  }
});
sendBtn.addEventListener("click", () => { if (turnRunning) stopTurn(); else sendMessage(); });

// ---- topbar / config ----------------------------------------------------------
// loadConfigLabels fills the topbar mode badge + the composer seats from the
// cached config. The topbar carries no model caption — dsh shows the model
// only in the composer ModelSelect seat.
function loadConfigLabels() {
  syncModeBadge();
  syncModelSeat();
  // The hero chip defaults to the runtime mode until the persisted agent_preset
  // arrives (loadComposerPrefs overrides it with the staged default).
  if (!mode) { mode = config.mode || ""; syncModeChip(); }
}
async function loadConfig() {
  try {
    const res = await api("/api/config");
    config = await res.json();
    if (Array.isArray(config.commands) && config.commands.length) {
      webCommands = config.commands;
    }
    if (slashMenuOpen) renderSlashMenu();
    loadConfigLabels();
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
}

// ---- settings page (P3: dsh SettingsRoot two-column panel, read-only) -------
// Section registry: general / model / caps / skills. Every control is read-only
// (ADR D-WEB2-D: no runtime editing — config changes need a restart). Icons are
// uniform 16px SVGs (dsh SettingsRoot navIcon: models → data outline, unknown →
// settings gear), so every nav glyph renders at the same size.
const SETTINGS_SECTIONS = [
  { id: "general", label: "通用设置", icon: PA_ICONS.settings },
  { id: "model", label: "模型", icon: PA_ICONS.data },
  { id: "caps", label: "能力开关", icon: PA_ICONS.personalization },
  { id: "skills", label: "技能", icon: PA_ICONS.skills },
];
const CAPABILITY_NAMES = {
  terminal: "终端", fs: "文件系统", fs_search: "全文检索", ralph: "Ralph 循环",
  workflow: "工作流", kb: "知识库", jobs: "后台任务", subagent: "子代理",
  web: "联网", eval: "评测", code: "代码执行", interact: "交互确认",
  mcp: "MCP", skill: "技能", schedule: "定时", plan: "计划",
  spill: "溢出", compaction: "压缩", multimodal: "多模态",
};
const MODEL_DISPLAY = { "deepseek-v4-flash": "DeepSeek-V4-Flash", "deepseek-v4-pro": "DeepSeek-V4-Pro" };
const PROVIDER_DISPLAY = { "deepseek-official": "DeepSeek" };

let settingsSec = "general";
let settingsConfig = null;

function settingsSectionEl() { return $("settings-sec"); }

function rowHTML(title, desc, control) {
  return `<div class="row">
    <div class="row-text"><div class="row-title">${esc(title)}</div>
    ${desc ? `<div class="row-desc">${esc(desc)}</div>` : ""}</div>
    ${control ? `<div class="row-control">${control}</div>` : ""}</div>`;
}

function renderSettingsNav() {
  const nav = $("settings-nav");
  nav.textContent = "";
  for (const s of SETTINGS_SECTIONS) {
    const btn = document.createElement("button");
    btn.className = "nav-cell" + (s.id === settingsSec ? " active" : "");
    btn.setAttribute("aria-current", s.id === settingsSec ? "true" : "false");
    btn.innerHTML = `<span class="nav-ico">${s.icon}</span><span>${esc(s.label)}</span>`;
    btn.addEventListener("click", () => { settingsSec = s.id; renderSettingsNav(); renderSettingsSec(); });
    nav.appendChild(btn);
  }
}

function renderGeneral(c) {
  const pref = localStorage.getItem(KEY_THEME) || "system";
  const cube = (id, label, icon) =>
    `<button class="theme-cube${pref === id ? " selected" : ""}" data-theme="${id}">${icon}<span>${label}</span></button>`;
  // dsh AppearanceRow: title above, the three theme cubes below.
  const appearance = `<div class="appearance-group">
    <div class="row-title">外观</div>
    <div class="theme-cubes">${cube("light", "浅色", PA_ICONS.light)}${cube("dark", "深色", PA_ICONS.dark)}${cube("system", "跟随系统", PA_ICONS.followsystem)}</div>
  </div>`;
  const enterMode = localStorage.getItem("pa_enter") || "send";
  // General-settings rows backed by the durable settings table (PATCH
  // /api/settings, applied at startup → restart required, D-WEB2-D). The
  // selectors fall back to localStorage while the API round-trip completes.
  const sel = (id, cur, opts) =>
    `<select id="${id}" class="row-select">${opts.map(([v, label]) => `<option value="${v}"${cur === v ? " selected" : ""}>${label}</option>`).join("")}</select>`;
  const ap = localStorage.getItem("pa_agent_preset") || "standard";
  const pp = localStorage.getItem("pa_permission_preset") || "standard";
  const ts = localStorage.getItem("pa_terminal_shell") || "off";
  const sec = settingsSectionEl();
  sec.innerHTML = `<h2>通用设置</h2>` +
    appearance +
    // dsh LanguageRow: title + selector pill (English is planned, not shipped).
    `<div class="settings-row">
      <div class="row-text"><div class="row-title">语言</div></div>
      <select id="lang-select" class="row-select">
        <option value="zh" selected>中文</option>
        <option value="en" disabled>English（规划中）</option>
      </select>
    </div>` +
    // dsh AgentPresetRow (数驼语义): the mode preset new sessions compose from.
    `<div class="settings-row">
      <div class="row-text"><div class="row-title">Agent 预设</div><div class="row-desc">新会话默认模式（极简 / 标准 / 编程），重启后生效。</div></div>
      ${sel("agent-preset-select", ap, [["minimal", "极简 minimal"], ["standard", "标准 standard"], ["code", "编程 code"]])}
    </div>` +
    // dsh PermissionRow (数驼语义): the tool-whitelist tier for new sessions.
    `<div class="settings-row">
      <div class="row-text"><div class="row-title">权限</div><div class="row-desc">新会话默认工具权限（只读 / 标准 / 全部），重启后生效。</div></div>
      ${sel("permission-select", pp, [["readonly", "只读"], ["standard", "标准"], ["full", "全部"]])}
    </div>` +
    // Default terminal (dsh 通用设置): pick the shell (Powershell / Git Bash
    // / WSL). Any choice except "关闭" enables the pwsh tool + /term (M9);
    // the shell selection configures the /term persistent session only — the
    // model's pwsh tool is a fresh pwsh process per call.
    `<div class="settings-row">
      <div class="row-text"><div class="row-title">默认终端</div><div class="row-desc">选择终端使用的 shell（PowerShell / Git Bash / WSL），重启后生效。</div></div>
      ${sel("terminal-select", ts, [["off", "关闭"], ["powershell", "PowerShell"], ["gitbash", "Git Bash"], ["wsl", "WSL"]])}
    </div>` +
    // dsh EnterBehaviorRow: title + description + selector pill.
    `<div class="settings-row">
      <div class="row-text"><div class="row-title">回车发送</div><div class="row-desc">Enter 直接发送，Shift+Enter 换行；或改为 Ctrl+Enter 发送。</div></div>
      <select id="enter-select" class="row-select">
        <option value="send"${enterMode === "send" ? " selected" : ""}>Enter 发送</option>
        <option value="newline"${enterMode === "newline" ? " selected" : ""}>Ctrl+Enter 发送</option>
      </select>
    </div>` +
    `<p class="notice">配置文件：config.yaml —— 修改后重启生效（无运行时热改）。</p>`;
  sec.querySelectorAll(".theme-cube").forEach((b) => {
    b.addEventListener("click", () => {
      localStorage.setItem(KEY_THEME, b.dataset.theme);
      applyTheme();
      renderGeneral(c);
    });
  });
  const enter = sec.querySelector("#enter-select");
  if (enter) enter.addEventListener("change", (e) => { localStorage.setItem("pa_enter", e.target.value); });
  // Durably persist the three host-backed rows on change.
  [["#agent-preset-select", "agent_preset"], ["#permission-select", "permission_preset"], ["#terminal-select", "terminal_shell"]]
    .forEach(([q, key]) => {
      const el = sec.querySelector(q);
      if (!el) return;
      el.addEventListener("change", async () => {
        localStorage.setItem("pa_" + key, el.value);
        try {
          const res = await api("/api/settings", { method: "PATCH", body: JSON.stringify({ [key]: el.value }) });
          if (res.status === 401) { showLogin("令牌无效或已过期"); return; }
          if (!res.ok) throw new Error("HTTP " + res.status);
          renderGeneral(c); // reflect the saved value
        } catch (e) { console.error("save setting", key, e); }
      });
    });
  // Backfill the stored values (and the in-effect values) from the backend.
  (async () => {
    try {
      const res = await api("/api/settings");
      const d = await res.json();
      if (d.agent_preset && sec.querySelector("#agent-preset-select")) sec.querySelector("#agent-preset-select").value = d.agent_preset;
      if (d.permission_preset && sec.querySelector("#permission-select")) sec.querySelector("#permission-select").value = d.permission_preset;
      if (d.terminal_shell && sec.querySelector("#terminal-select")) sec.querySelector("#terminal-select").value = d.terminal_shell;
      // The persistent-shell tool row carries the chosen shell's name (dsh
      // pwsh / Git Bash / WSL rows).
      if (d.terminal_shell && SHELL_TITLES[d.terminal_shell]) termShellTitle = SHELL_TITLES[d.terminal_shell];
    } catch (e) { if (e.message !== "unauthorized") console.error("load settings", e); }
  })();
}

// Model settings page (M11: dsh ModelsSection) — provider row-cards, one per
// known provider: every built-in (deepseek-official always; openai/anthropic even when
// their env key is absent) plus every M11 custom OpenAI-compatible provider.
// Rows render only for configured/custom providers (dormant built-ins — known
// but no key — are NOT rows; they are reached through the 增加提供方 add card).
// Each row shows the display name, a 自定义 tag for custom providers, the active
// tag, a credential dot (configured = a key is present in settings or env →
// green, missing → red), the configured model, and actions: 编辑 (opens the
// ProviderEditor: API Key primary + collapsed 自定义设置) and 删除 (custom only).
// The 增加提供方 add card is dsh's add flow: a provider <select> of the dormant
// built-ins + the editor for the chosen one, so adding = pick an existing
// provider then just enter its API key. 增加自定义提供方 declares a brand-new
// OpenAI-compatible endpoint. Keys default from the environment variable; a key
// entered here takes precedence (配置后以配置的为准, user 2026-09).
const PROVIDER_ENV = { "deepseek-official": "DEEPSEEK_API_KEY", openai: "OPENAI_API_KEY", anthropic: "ANTHROPIC_API_KEY" };
let modelEditing = null;  // provider id open in its full editor card (row 编辑)
let adding = false;       // true while the 增加提供方 add card is open
let addingId = null;      // provider id selected in the add card's <select>
let customAdding = false; // true while the 增加自定义提供方 create card is open
let savedNotice = null;   // persistent "已保存 <provider>。" notice (dsh savedNotice 对齐)

// M11-pi-ai multi-model list editor (dsh ModelListEditor 对齐). The draft
// model rows of the open custom card live here so re-renders keep them; the
// picker modal is a temporary overlay appended to document.body.
let modelDraft = [];   // [{id,name,context_window,max_tokens}]
// providerFormDraft keeps the unsaved form values of the open editor card
// across the model-list re-renders (dsh ProviderEditor: the card is a
// controlled form, so adding/removing a model row or adopting probe results
// never loses the other fields). key/base cover the provider edit card;
// custom carries the 增加自定义提供方 card's route/name/base/protocol/key.
// Reset when the editor closes or switches provider.
let providerFormDraft = { key: "", base: "", custom: { route: "", name: "", base: "", protocol: "openai-completions", key: "" } };
let probeOpen = false; // true while the 获取可用模型 picker overlay is up

// modelListHTML renders the ModelListEditor into an open card: one row per
// draft entry (id + optional name, collapsed 容量 with context_window /
// max_tokens), plus the 添加模型 and 获取可用模型 actions. The rows are only
// ever added deliberately — an empty list means "none yet".
function modelListHTML() {
  const row = (m, i) => `<div class="m-modelrow" data-i="${i}">
      <input class="m-input m-modelid" value="${esc(m.id || "")}" placeholder="模型 ID" autocomplete="off">
      <input class="m-input m-modelname" value="${esc(m.name || "")}" placeholder="名称（可选）" autocomplete="off">
      <button type="button" class="m-btn m-secondary m-modeldel" title="移除该模型">移除</button>
      <details class="m-customized">
        <summary class="m-customizedsummary">容量</summary>
        <div class="m-customizedbody">
          <input class="m-input m-modelctx" type="number" min="0" value="${m.context_window || ""}" placeholder="上下文窗口">
          <input class="m-input m-modeltok" type="number" min="0" value="${m.max_tokens || ""}" placeholder="最大输出">
        </div>
      </details>
    </div>`;
  return `<div class="m-modellist">
    <div class="m-modellistrows">${modelDraft.map(row).join("")}</div>
    <div class="m-modellistactions">
      <button type="button" class="m-btn m-secondary" id="m-model-add">添加模型</button>
      <button type="button" class="m-btn m-secondary" id="m-model-probe">获取可用模型</button>
    </div>
  </div>`;
}

// readModelDraft syncs the rendered rows back into modelDraft (id/name + the
// collapsed capacities), dropping fully-blank rows.
function readModelDraft(sec) {
  const rows = sec.querySelectorAll(".m-modelrow");
  const next = [];
  rows.forEach((r) => {
    const id = (r.querySelector(".m-modelid").value || "").trim();
    const name = (r.querySelector(".m-modelname").value || "").trim();
    const ctx = parseInt(r.querySelector(".m-modelctx").value, 10);
    const tok = parseInt(r.querySelector(".m-modeltok").value, 10);
    if (!id) return;
    next.push({ id, name, context_window: ctx > 0 ? ctx : undefined, max_tokens: tok > 0 ? tok : undefined });
  });
  modelDraft = next;
}

// wireModelList binds the model editor's actions in the card currently shown.
// probeCtx supplies the live form's base URL / protocol / key for the 获取可用
// 模型 action (dsh: ask the endpoint the form currently shows, key included).
function wireModelList(sec, probeCtx) {
  // Keep the unsaved key / base values across the re-render a row edit causes
  // (dsh ProviderEditor controlled form).
  const saveFormDraft = () => {
    const k = sec.querySelector("#m-provider-key");
    const b = sec.querySelector("#m-provider-base");
    if (k) providerFormDraft.key = k.value;
    if (b) providerFormDraft.base = b.value;
  };
  const addBtn = sec.querySelector("#m-model-add");
  const probeBtn = sec.querySelector("#m-model-probe");
  if (addBtn) addBtn.addEventListener("click", () => { saveFormDraft(); modelDraft.push({ id: "", name: "" }); renderModel(config); });
  sec.querySelectorAll(".m-modeldel").forEach((btn) => {
    btn.addEventListener("click", () => { saveFormDraft(); const i = Number(btn.closest(".m-modelrow").dataset.i); modelDraft.splice(i, 1); renderModel(config); });
  });
  if (probeBtn) probeBtn.addEventListener("click", () => { saveFormDraft(); void probeModels(sec, probeCtx); });
}

// probeModels asks the endpoint (POST /api/config/provider/discover) which
// models it advertises and opens the picker. probeCtx reads the live form:
// base URL, protocol, and a key typed but not yet saved. A built-in provider
// (directory route) answers from its own catalog without any endpoint — the
// base_url and key requirements apply only to hand-declared custom endpoints:
// probing one needs both the API 地址 and the API Key.
async function probeModels(sec, ctx) {
  if (probeOpen) return;
  const base = (ctx.baseEl ? ctx.baseEl.value : "").trim();
  const key = (ctx.keyEl ? ctx.keyEl.value : "").trim();
  if (!ctx.directory) {
    if (!base) { alert("请先填写 API 地址再获取可用模型"); return; }
    if (!key) { alert("请先输入 API Key 再获取可用模型"); return; }
  }
  const protocol = ctx.protocolEl ? ctx.protocolEl.value : (ctx.protocol || "openai-completions");
  const provider = ctx.provider || "";
  probeOpen = true;
  try {
    const res = await api("/api/config/provider/discover", { method: "POST", body: JSON.stringify({ provider, base_url: base, protocol, api_key: key }) });
    if (res.status === 401) { showLogin("令牌无效或已过期"); return; }
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
    openProbePicker(data.models || []);
  } catch (e) {
    alert("获取可用模型失败：" + e.message);
  } finally { probeOpen = false; }
}

// openProbePicker renders the adoption modal: checkbox list of the candidates
// the endpoint reported. Candidates already configured (a draft row with the
// same id) start unchecked — adopting a selection never overwrites a capacity
// the user corrected. 全选 / 取消全选 change only the checkboxes; nothing is
// written until 添加所选.
function openProbePicker(candidates) {
  if (candidates.length === 0) { alert("该端点未返回任何模型。"); return; }
  const existing = new Set(modelDraft.map((m) => m.id));
  const overlay = document.createElement("div");
  overlay.className = "probe-overlay";
  overlay.innerHTML = `<div class="probe-modal">
    <div class="probe-head">
      <span class="probe-title">选择要添加的模型</span>
      <button type="button" class="probe-close" aria-label="关闭">✕</button>
    </div>
    <div class="probe-tools">
      <button type="button" class="m-btn m-secondary" id="probe-all">全选</button>
      <button type="button" class="m-btn m-secondary" id="probe-none">取消全选</button>
    </div>
    <ul class="probe-list">
      ${candidates.map((c) => `<li><label><input type="checkbox" value="${esc(c.id)}" ${existing.has(c.id) ? "" : "checked"}> <span class="probe-id">${esc(c.id)}</span>${c.name ? `<span class="probe-name">${esc(c.name)}</span>` : ""}</label></li>`).join("")}
    </ul>
    <div class="probe-actions">
      <button type="button" class="m-btn m-secondary" id="probe-cancel">取消</button>
      <button type="button" class="m-btn m-primary" id="probe-adopt">添加所选</button>
    </div>
  </div>`;
  document.body.appendChild(overlay);
  const close = () => overlay.remove();
  overlay.querySelector(".probe-close").addEventListener("click", close);
  overlay.addEventListener("click", (e) => { if (e.target === overlay) close(); });
  overlay.querySelector("#probe-cancel").addEventListener("click", close);
  overlay.querySelector("#probe-all").addEventListener("click", () => overlay.querySelectorAll('input[type="checkbox"]').forEach((b) => { b.checked = true; }));
  overlay.querySelector("#probe-none").addEventListener("click", () => overlay.querySelectorAll('input[type="checkbox"]').forEach((b) => { b.checked = false; }));
  overlay.querySelector("#probe-adopt").addEventListener("click", () => {
    const picked = new Set();
    overlay.querySelectorAll('input[type="checkbox"]:checked').forEach((b) => picked.add(b.value));
    for (const c of candidates) {
      if (!picked.has(c.id)) continue;
      // Never overwrite an existing row's capacity — only add the new ids.
      if (existing.has(c.id)) continue;
      modelDraft.push({ id: c.id, name: c.name || "", context_window: c.context_window || undefined, max_tokens: c.max_tokens || undefined });
    }
    close();
    renderModel(config);
  });
}

const providerLabel = (p) => {
  const d = PROVIDER_DISPLAY[p.id] || p.name || p.id;
  return d === p.id ? d : d + " (" + p.id + ")";
};

function renderModel(c) {
  const sec = settingsSectionEl();
  const providers = (c.providers || []).slice();
  const currentProvider = c.llm_provider || "deepseek-official";
  const currentModel = c.model || "";
  // configured-first (dsh sorts usable providers up), then registered; the
  // active one keeps its place among the configured rows.
  const sorted = providers.sort((a, b) => (Number(b.configured) - Number(a.configured)) || (Number(b.registered) - Number(a.registered)));
  // Canonical credential env var comes from the backend directory (M11-pi-ai:
  // HF_TOKEN, KIMI_API_KEY, AI_GATEWAY_API_KEY, ...); fall back to the derived
  // <UPPER_ROUTE>_API_KEY for custom providers.
  const envName = (p) => p.env_var || PROVIDER_ENV[p.id] || (p.id.toUpperCase().replace(/-/g, "_") + "_API_KEY");
  // Dormant built-in providers = known but not yet registered (no key): these
  // are the dsh "addable" providers offered by the 增加提供方 add card.
  const dormant = sorted.filter((p) => !p.custom && !p.registered);
  const rows = sorted.filter((p) => p.custom || p.registered);

  // dsh ProviderEditor: the primary field is the API key; the collapsed
  // 自定义设置 details carries the per-family extras (API 地址 + 模型目录).

  // Provider-row model summary: the profile's model list when overridden
  // (dsh ProviderEditor 自定义设置), else every candidate — with the current
  // one bolded (dsh ModelSeat 对齐). The row previously showed only the active
  // model, so a provider with two models looked like it offered just one.
  const modelRowLabel = (p) => {
    const fromProfile = (p.models && p.models.length) ? p.models.map((m) => m.id || m) : null;
    const cands = fromProfile || (p.candidates && p.candidates.length ? p.candidates : (p.model ? [p.model] : []));
    const cur = p.model || "";
    return cands.map((m) => m === cur ? `<b>${esc(MODEL_DISPLAY[m] || m)}</b>` : esc(MODEL_DISPLAY[m] || m)).join(" · ") || "";
  };

  // The model-directory snapshot a provider editor was seeded with (its
  // persisted profile models, else the directory candidates) — the baseline
  // "未发生变化" compares the edited draft against.
  const seedModelsFor = (p) => {
    if (!p) return [];
    const seed = (p.models && p.models.length) ? p.models : (p.candidates || []).map((m) => ({ id: m, name: "" }));
    return seed.map((m) => ({ id: m.id || m, name: m.name || "", context_window: m.context_window || undefined, max_tokens: m.max_tokens || undefined }));
  };

  const editorHTML = (p, mode) => {
    const name = PROVIDER_DISPLAY[p.id] || p.name || p.id;
    const active = p.id === currentProvider;
    const title = mode === "add" ? `增加 ${esc(providerLabel(p))}` : `编辑 ${esc(providerLabel(p))}`;
    const submitLabel = mode === "add" ? "保存" : "应用";
    return `<div class="m-editor">
      <div class="m-editorhead">
        <span class="m-editortitle">${title}</span>
        <span class="m-editorroute">${esc(p.id)}${p.custom && p.protocol_label ? ` · ${esc(p.protocol_label)}` : ""}</span>
      </div>
      <div class="m-field">
        <span class="m-fieldlabel">API Key</span>
        <input id="m-provider-key" class="m-input" type="password" autocomplete="off" placeholder="留空使用环境变量 ${esc(envName(p))}" value="${esc(providerFormDraft.key)}">
        <span class="m-fieldhint">Key 默认读取环境变量 ${esc(envName(p))}；填入后以配置值为准（留空并保存即清除自定义 Key，回到环境变量）。</span>
      </div>
      <details class="m-customized">
        <summary class="m-customizedsummary">自定义设置</summary>
        <div class="m-customizedbody">
          <div class="m-field">
            <span class="m-fieldlabel">API 地址</span>
            <input id="m-provider-base" class="m-input" value="${esc(providerFormDraft.base !== "" ? providerFormDraft.base : (p.base_url || ""))}" placeholder="${p.custom ? "https://api.example.com/v1" : "https://api.deepseek.com（提供方默认）"}">
          </div>
          <div class="m-field">
            <span class="m-fieldlabel">模型</span>
            ${modelListHTML()}
          </div>
        </div>
      </details>
      ${p.registered && !p.available ? `<p class="m-notice">当前不可用（Key 缺失或 API 地址无效）。</p>` : ""}
      <div class="m-editoractions">
        <button type="button" class="m-btn m-secondary" id="m-model-cancel">取消</button>
        <button type="button" class="m-btn m-primary" id="m-model-apply">${submitLabel}</button>
        <span id="m-model-status" class="model-status"></span>
      </div>
    </div>`;
  };

  let t = `<h2>模型</h2>
    <p class="intro">配置 API Key 后即可使用以下提供方。切换提供方 / 模型即时生效（下一条消息即用新模型）；Key 默认从环境变量读取，在本页填入的 Key 以配置值为准（覆盖环境变量）。</p>`;
  if (savedNotice) t += `<p class="m-saved-notice" role="status" aria-live="polite">${esc(savedNotice)}</p>`;
  t += `<ul class="m-rows">`;

  for (const p of rows) {
    const name = providerLabel(p);
    const active = p.id === currentProvider;
    if (modelEditing === p.id) {
      t += `<li class="m-rowcard m-editing">${editorHTML(p, "edit")}</li>`;
    } else {
      t += `<li class="m-rowcard">
        <div class="m-rowhead">
          <span class="m-rowid">
            <span class="m-rowname">${esc(name)}</span>
            ${active ? `<span class="m-rowtag current">当前</span>` : ""}
            ${p.custom ? `<span class="m-rowtag custom">自定义</span>` : ""}
            <span class="m-dot ${p.configured ? "configured" : "missing"}" title="${p.configured ? "API Key 已配置" : "未配置（缺 API Key）"}"></span>
          </span>
          <span class="m-rowmodel muted">${modelRowLabel(p)}</span>
          <span class="m-rowactions">
            <button type="button" class="m-btn m-secondary" data-edit="${esc(p.id)}">编辑</button>
            ${p.custom ? `<button type="button" class="m-btn m-secondary m-danger" data-del="${esc(p.id)}">删除</button>` : ""}
          </span>
        </div>
      </li>`;
    }
  }

  // 增加提供方 add card (dsh addBlock): provider <select> of dormant built-ins
  // + the editor for the chosen one. This is dsh's add flow — pick an existing
  // provider, then just enter its API key.
  if (adding) {
    const target = dormant.find((x) => x.id === addingId) || dormant[0];
    t += `<li class="m-rowcard m-editing">
      <div class="m-editor">
        <div class="m-editorhead">
          <span class="m-editortitle">增加提供方</span>
          <span class="m-editorroute">选择已有提供方</span>
        </div>
        ${dormant.length
          ? `<div class="m-field">
              <span class="m-fieldlabel">提供方</span>
              <select id="m-add-provider-select" class="m-input m-select">
                ${dormant.map((p) => `<option value="${esc(p.id)}" ${p.id === (target && target.id) ? "selected" : ""}>${esc(PROVIDER_DISPLAY[p.id] || p.name || p.id)}${p.protocol_label ? "（" + esc(p.protocol_label) + "）" : ""}</option>`).join("")}
              </select>
            </div>
            ${target ? editorHTML(target, "add") : ""}`
          : `<p class="m-notice">没有可增加的内置提供方（已全部配置）。可改用「增加自定义提供方」。</p>`}
        <div class="m-editoractions">
          <button type="button" class="m-btn m-secondary" id="m-add-cancel">取消</button>
        </div>
      </div>
    </li>`;
  }

  // 增加自定义提供方 create card (dsh CustomProviderCard 对齐): route id being
  // chosen here (not edited later), optional display name (defaults to the
  // route), base URL, wire protocol (四协议), model, and the API key. The
  // route and key fields validate inline as you type (dsh ROUTE_PATTERN +
  // apiKeyFailure 范式).
  const ROUTE_PATTERN = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/;
  const LEGAL_API_KEY = /^[\x21-\x7E]+$/;
  const ENV_LINE = /^[A-Z][A-Z0-9_]*=[^=]/;
  const isQuotedKey = (v) => {
    const f = v[0];
    if (f !== '"' && f !== "'" && f !== "`") return false;
    return v.length > 1 && v.endsWith(f);
  };
  const apiKeyFailure = (draft) => {
    if (draft.length === 0) return undefined;
    const value = draft.trim();
    if (value.length === 0) return "keyBlank";
    if (ENV_LINE.test(value) || isQuotedKey(value)) return "keyIllegalCharacters";
    if (!LEGAL_API_KEY.test(value)) return "keyIllegalCharacters";
    return undefined;
  };
  const PROTOCOL_OPTIONS = [
    ["openai-completions", "OpenAI 兼容"],
    ["anthropic-messages", "Anthropic Messages"],
    ["google-generative-ai", "Google Generative AI"],
    ["openai-responses", "OpenAI Responses"],
  ];
  if (customAdding) {
    const cd = providerFormDraft.custom || { route: "", name: "", base: "", protocol: "openai-completions", key: "" };
    t += `<li class="m-rowcard m-editing">
      <div class="m-editor">
        <div class="m-editorhead">
          <span class="m-editortitle">增加自定义提供方</span>
          <span class="m-editorroute">声明一个自定义端点</span>
        </div>
        <div class="m-field">
          <span class="m-fieldlabel">路由 ID</span>
          <input id="m-custom-route" class="m-input" value="${esc(cd.route)}" placeholder="acme-gateway" autocomplete="off">
        </div>
        <p id="m-custom-route-msg" class="m-fieldhint">小写字母开头，仅小写字母 / 数字 / 单个连字符分隔（如 acme-gateway）。</p>
        <div class="m-field">
          <span class="m-fieldlabel">显示名称</span>
          <input id="m-custom-name" class="m-input" value="${esc(cd.name)}" placeholder="留空使用路由 ID" autocomplete="off">
        </div>
        <div class="m-field">
          <span class="m-fieldlabel">API 地址</span>
          <input id="m-custom-base" class="m-input" value="${esc(cd.base)}" placeholder="https://gateway.example/v1" autocomplete="off">
        </div>
        <div class="m-field">
          <span class="m-fieldlabel">协议</span>
          <select id="m-custom-protocol" class="m-input m-select">
            ${PROTOCOL_OPTIONS.map(([v, label]) => `<option value="${v}"${cd.protocol === v ? " selected" : ""}>${esc(label)}（${v}）</option>`).join("")}
          </select>
        </div>
        <div class="m-field">
          <span class="m-fieldlabel">模型</span>
          ${modelListHTML()}
        </div>
        <div class="m-field">
          <span class="m-fieldlabel">API Key</span>
          <input id="m-custom-key" class="m-input" type="password" autocomplete="off" value="${esc(cd.key)}" placeholder="留空使用环境变量">
        </div>
        <p id="m-custom-key-msg" class="m-fieldhint">Key 默认读取环境变量（大写路由名 + _API_KEY，如 ACME_GATEWAY_API_KEY）；填入后以配置值为准。</p>
        <div class="m-editoractions">
          <button type="button" class="m-btn m-secondary" id="m-model-cancel">取消</button>
          <button type="button" class="m-btn m-primary" id="m-model-apply">创建</button>
          <span id="m-model-status" class="model-status"></span>
        </div>
      </div>
    </li>`;
  }

  // dsh addActions: two add buttons below the rows.
  t += `</ul>
    <div class="m-addrow">
      <button type="button" class="m-btn m-add" id="m-add-provider" ${dormant.length === 0 ? "disabled" : ""}>增加提供方</button>
      <button type="button" class="m-btn m-add" id="m-add-custom">增加自定义提供方</button>
    </div>
    <p class="notice">API Key 默认从环境变量读取（不落 config.yaml）；本页填入的 Key 以配置值为准并持久化保存。修改 config.yaml 重启后回到文件配置。</p>`;
  sec.innerHTML = t;

  // Open the full editor card for a provider row.
  sec.querySelectorAll("[data-edit]").forEach((btn) => {
    btn.addEventListener("click", () => {
      savedNotice = null; modelEditing = btn.dataset.edit; adding = false; addingId = null; customAdding = false;
      providerFormDraft = { key: "", base: "" };
      // Both custom and built-in edit cards show the multi-model list
      // (dsh ProviderEditor 自定义设置): seed the draft from the persisted
      // profile models when present, else the provider directory candidates
      // (a built-in without a profile starts from its catalog).
      const p = providers.find((x) => x.id === btn.dataset.edit);
      if (p) {
        const seed = (p.models && p.models.length) ? p.models : (p.candidates || []).map((m) => ({ id: m, name: "" }));
        modelDraft = seed.length ? seed.map((m) => ({ id: m.id, name: m.name || "", context_window: m.context_window || undefined, max_tokens: m.max_tokens || undefined })) : [{ id: p.model || "", name: "" }];
      }
      renderModel(c);
    });
  });
  // Delete a custom provider (dsh remove, with confirm).
  sec.querySelectorAll("[data-del]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const id = btn.dataset.del;
      if (!confirm(`删除自定义提供方 ${id}？`)) return;
      savedNotice = null;
      try {
        const res = await api("/api/config/provider", { method: "DELETE", body: JSON.stringify({ id }) });
        if (res.status === 401) { showLogin("令牌无效或已过期"); return; }
        if (!res.ok) { const eb = await res.json().catch(() => ({})); throw new Error(eb.error || ("HTTP " + res.status)); }
        if (modelEditing === id) modelEditing = null;
        await loadConfig();
        renderModel(config);
      } catch (e) { alert("删除失败：" + e.message); }
    });
  });
  // 增加提供方: open the add card (dormant provider <select> + editor).
  const addProvider = sec.querySelector("#m-add-provider");
  if (addProvider) {
    addProvider.addEventListener("click", () => {
      savedNotice = null; adding = true; addingId = dormant[0] ? dormant[0].id : null; modelEditing = null; customAdding = false; renderModel(c);
    });
  }
  // Switching the provider <select> in the add card re-targets the editor.
  const addSelect = sec.querySelector("#m-add-provider-select");
  if (addSelect) {
    addSelect.addEventListener("change", () => { addingId = addSelect.value; renderModel(c); });
  }
  const addCancel = sec.querySelector("#m-add-cancel");
  if (addCancel) {
    addCancel.addEventListener("click", () => { adding = false; addingId = null; renderModel(c); });
  }
  const addCustom = sec.querySelector("#m-add-custom");
  if (addCustom) {
    addCustom.addEventListener("click", () => {
      savedNotice = null; modelEditing = null; adding = false; addingId = null; customAdding = true;
      modelDraft = [{ id: "", name: "" }]; renderModel(c);
    });
  }

  // Wire the editor / custom-create actions.
  const apply = sec.querySelector("#m-model-apply");
  if (!apply) return;
  const cancel = sec.querySelector("#m-model-cancel");
  const status = sec.querySelector("#m-model-status");
  cancel.addEventListener("click", () => { modelEditing = null; customAdding = false; adding = false; addingId = null; providerFormDraft = { key: "", base: "" }; renderModel(c); });

  if (customAdding) {
    // Live inline validation (dsh apiKeyFailure / ROUTE_PATTERN 范式): the
    // route and key fields report their own fault beneath themselves; the
    // submit button stays disabled until the whole card is ready.
    const routeMsg = sec.querySelector("#m-custom-route-msg");
    const keyMsg = sec.querySelector("#m-custom-key-msg");
    const routeInput = sec.querySelector("#m-custom-route");
    const keyInput = sec.querySelector("#m-custom-key");
    const customRouteMsg = (route) => {
      if (route.length === 0) return { ok: false, msg: "小写字母开头，仅小写字母 / 数字 / 单个连字符分隔（如 acme-gateway）。" };
      if (!ROUTE_PATTERN.test(route)) return { ok: false, msg: "路由 ID 非法：小写字母开头，仅小写字母 / 数字 / 单个连字符分隔。" };
      if (providers.some((p) => p.id === route)) return { ok: false, msg: "该路由 ID 已被占用。" };
      return { ok: true, msg: "" };
    };
    const refreshCustomState = () => {
      readModelDraft(sec);
      const r = customRouteMsg(routeInput.value.trim());
      const kf = apiKeyFailure(keyInput.value);
      routeMsg.textContent = r.msg || "小写字母开头，仅小写字母 / 数字 / 单个连字符分隔（如 acme-gateway）。";
      routeMsg.classList.toggle("m-fielderror", r.msg.startsWith("路由 ID 非法") || r.msg.startsWith("该路由 ID"));
      keyMsg.textContent = kf === "keyBlank" ? "Key 不能为纯空白。" : kf === "keyIllegalCharacters" ? "Key 含非法字符（不能有环境变量行、引号或非打印字符）。" : "Key 默认读取环境变量（大写路由名 + _API_KEY，如 ACME_GATEWAY_API_KEY）；填入后以配置值为准。";
      keyMsg.classList.toggle("m-fielderror", !!kf);
      const ready = r.ok && sec.querySelector("#m-custom-base").value.trim().length > 0
        && modelDraft.length > 0 && !kf;
      apply.disabled = !ready;
      // Keep the typed values across re-renders (probe adoption / row edits).
      providerFormDraft.custom = {
        route: routeInput.value, name: sec.querySelector("#m-custom-name").value,
        base: sec.querySelector("#m-custom-base").value, protocol: sec.querySelector("#m-custom-protocol").value,
        key: keyInput.value,
      };
    };
    routeInput.addEventListener("input", refreshCustomState);
    keyInput.addEventListener("input", refreshCustomState);
    sec.querySelector("#m-custom-name").addEventListener("input", refreshCustomState);
    sec.querySelector("#m-custom-base").addEventListener("input", refreshCustomState);
    sec.querySelector("#m-custom-protocol").addEventListener("change", refreshCustomState);
    // The model list is a self-contained editor: its rows live in modelDraft.
    wireModelList(sec, { baseEl: sec.querySelector("#m-custom-base"), protocolEl: sec.querySelector("#m-custom-protocol"), keyEl: keyInput });
    refreshCustomState();
    // 增加自定义提供方: POST /api/config/provider {custom:true, ...}
    apply.addEventListener("click", async () => {
      readModelDraft(sec);
      const route = routeInput.value.trim();
      const name = (sec.querySelector("#m-custom-name").value || "").trim();
      const base = (sec.querySelector("#m-custom-base").value || "").trim();
      const protocol = sec.querySelector("#m-custom-protocol").value;
      const key = keyInput.value.trim();
      const r = customRouteMsg(route);
      if (!r.ok) { status.textContent = r.msg; return; }
      if (!base || modelDraft.length === 0) { status.textContent = "API 地址 / 模型必填"; return; }
      const kf = apiKeyFailure(keyInput.value);
      if (kf) { status.textContent = kf === "keyBlank" ? "Key 不能为纯空白" : "Key 含非法字符"; return; }
      status.textContent = "保存中…";
      apply.disabled = true;
      try {
        const res = await api("/api/config/provider", { method: "POST", body: JSON.stringify({
          id: route, name, base_url: base, models: modelDraft, protocol, api_key: key, custom: true,
        }) });
        if (res.status === 401) { showLogin("令牌无效或已过期"); return; }
        if (!res.ok) { const eb = await res.json().catch(() => ({})); throw new Error(eb.error || ("HTTP " + res.status)); }
        modelEditing = null; customAdding = false;
        await loadConfig();
        const savedP = (config.providers || []).find((x) => x.id === route) || { id: route, name };
        savedNotice = "已保存 " + providerLabel(savedP) + "。";
        renderModel(config);
      } catch (e) {
        status.textContent = "失败：" + e.message;
      } finally { apply.disabled = false; }
    });
    return;
  }

  const editId = modelEditing || addingId;
  const target = (modelEditing ? sorted : dormant).find((x) => x.id === editId);
  const active = editId === currentProvider;
  const isCustomEdit = modelEditing && target && target.custom;
  const keyInput = sec.querySelector("#m-provider-key");
  const baseInput = sec.querySelector("#m-provider-base");
  const isAdd = adding && !modelEditing;
  // Both custom and built-in editors show the multi-model list (M11-pi-ai /
  // dsh ProviderEditor 自定义设置): wire its rows + probe, seeded from the
  // provider's persisted models / directory candidates. Protocol is fixed at
  // create time (dsh: route chosen at create); the probe passes it so an
  // OpenAI-compatible custom route can still be interrogated.
  if (modelEditing && target) {
    wireModelList(sec, {
      baseEl: baseInput,
      keyEl: keyInput,
      provider: target.id,
      // A built-in provider is a directory route: probing answers from its
      // catalog without an endpoint, so no base URL is required.
      directory: !target.custom,
      ...target.custom ? { protocol: target.protocol || "openai-completions" } : {},
    });
  }
  apply.addEventListener("click", async () => {
    readModelDraft(sec);
    const body = {};
    if (!active && !isAdd) body.provider = editId;
    // The model list's first row is the effective default model.
    const first = modelDraft.length ? modelDraft[0].id : "";
    if (first && first !== (active ? currentModel : (target && target.model))) body.model = first;
    const key = keyInput.value.trim();
    if (key) body.api_key = key;
    // 自定义设置 (dsh ProviderEditor): API 地址 + 模型目录, saved for built-ins
    // as a profile override and for custom rows as their full profile.
    const base = baseInput ? baseInput.value.trim() : "";
    const baseChanged = base !== (target && target.base_url || "");
    const modelsChanged = JSON.stringify(modelDraft) !== JSON.stringify(seedModelsFor(target));
    const profileChanged = baseChanged || modelsChanged;
    if (profileChanged) {
      body.base_url = base;
      body.models = modelDraft;
    }
    if (isAdd) {
      // dsh add flow: register the dormant provider with its key (nothing else
      // changes for a built-in — model/API 地址 come from the config defaults).
      if (!key) { status.textContent = "请输入 API Key（留空没有意义）"; return; }
      status.textContent = "保存中…";
      apply.disabled = true;
      try {
        const res = await api("/api/config/provider", { method: "POST", body: JSON.stringify({ id: editId, api_key: key }) });
        if (res.status === 401) { showLogin("令牌无效或已过期"); return; }
        if (!res.ok) { const eb = await res.json().catch(() => ({})); throw new Error(eb.error || ("HTTP " + res.status)); }
        adding = false; addingId = null;
        await loadConfig();
        const savedP = (config.providers || []).find((x) => x.id === editId) || target;
        savedNotice = "已保存 " + providerLabel(savedP) + "。";
        renderModel(config);
      } catch (e) {
        status.textContent = "失败：" + e.message;
      } finally { apply.disabled = false; }
      return;
    }
    if (!body.provider && !body.model && !body.api_key && !profileChanged) { status.textContent = "未发生变化"; return; }
    status.textContent = "应用中…";
    apply.disabled = true;
    try {
      // Key / profile (M11 / dsh ProviderEditor 自定义设置) persists via the
      // provider API first, then the model switch applies live (P5.1).
      if (body.api_key || profileChanged || (target && target.custom && body.model)) {
        const rk = await api("/api/config/provider", {
          method: "POST",
          body: JSON.stringify({
            id: editId,
            ...target && target.custom ? { name: target.name, protocol: target.protocol || "openai-completions", custom: true } : {},
            ...(body.base_url !== undefined ? { base_url: body.base_url } : target && target.custom ? { base_url: target.base_url } : {}),
            ...(body.model !== undefined ? { model: body.model } : target && target.custom ? { model: target.model } : {}),
            ...(body.models !== undefined ? { models: body.models } : target && target.custom ? { models: modelDraft } : {}),
            api_key: key,
          }),
        });
        if (rk.status === 401) { showLogin("令牌无效或已过期"); return; }
        if (!rk.ok) { const eb = await rk.json().catch(() => ({})); throw new Error(eb.error || ("HTTP " + rk.status)); }
      }
      if (body.provider || body.model) {
        const rm = await api("/api/config/model", { method: "POST", body: JSON.stringify(body) });
        if (rm.status === 401) { showLogin("令牌无效或已过期"); return; }
        if (!rm.ok) { const eb = await rm.json().catch(() => ({})); throw new Error(eb.error || ("HTTP " + rm.status)); }
      }
      await loadConfig();        // refresh the config view (model/provider)
      loadConfigLabels();        // update the sidebar mode/model badge
      const savedP = (config.providers || []).find((x) => x.id === editId) || target;
      savedNotice = "已保存 " + providerLabel(savedP) + "。";
      modelEditing = null;       // close the editor (dsh closeEditor)
      providerFormDraft = { key: "", base: "" };
      renderModel(config);       // re-render with the new selection
    } catch (e) {
      status.textContent = "失败：" + e.message;
    } finally {
      apply.disabled = false;
    }
  });
}

function renderCaps(c) {
  const rows = [];
  for (const k of Object.keys(c)) {
    if (!k.endsWith("_enabled") || typeof c[k] !== "boolean") continue;
    const short = k.replace(/_enabled$/, "");
    rows.push([CAPABILITY_NAMES[short] || short, k, c[k]]);
  }
  rows.sort((a, b) => a[0].localeCompare(b[0]));
  const sec = settingsSectionEl();
  sec.innerHTML = `<h2>能力开关</h2>
    <p class="intro">各能力默认关闭（D10），启用需在 config.yaml 打开对应开关。</p>
    <p class="notice">修改 config.yaml 后重启生效（无运行时热改）。</p>`;
  for (const [name, key, on] of rows) {
    const d = document.createElement("div");
    d.innerHTML = rowHTML(name, `config 键：${esc(key)}`, `<span class="cap-badge ${on ? "on" : ""}">${on ? "开" : "关"}</span>`);
    sec.appendChild(d.firstElementChild);
  }
}

// ---- 技能 settings page (dsh-skill-mcp-panel 对齐; user 2026-09) -----------
// Boots via GET /api/config/skills (list + groups + scopes in one round trip),
// then drives every action through POST /api/config/skills { action }.
let skillState = { skills: [], groups: [], scopes: [], enabled: false, query: "", scopeFilter: "all", groupFilter: "", expanded: null };

function skillScopeLabel(id) {
  const hit = (skillState.scopes || []).find((s) => s.id === id);
  return hit ? hit.label : id;
}
function skillGroupNames(ep) {
  // A skill's groups for its scope: collect from group rows whose scope map lists the name.
  const out = [];
  for (const g of skillState.groups) {
    const names = (g.scopes && g.scopes[ep.scope]) || [];
    if (names.includes(ep.name)) out.push(g.name);
  }
  return out;
}

async function skillAPI(action, payload) {
  const res = await api("/api/config/skills", { method: "POST", body: JSON.stringify(Object.assign({ action }, payload)) });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || ("请求失败 " + res.status));
  }
  return await res.json();
}

function skillOpts(ep) {
  const on = ep.enabled;
  const toggle = `<button class="sk-btn" data-skill-toggle="${esc(ep.name)}" data-skill-scope="${esc(ep.scope)}" data-skill-on="${on ? "0" : "1"}">${on ? "停用" : "启用"}</button>`;
  const del = `<button class="sk-btn danger" data-skill-delete="${esc(ep.name)}" data-skill-scope="${esc(ep.scope)}">删除</button>`;
  const mig = `<button class="sk-btn" data-skill-migrate="${esc(ep.name)}" data-skill-scope="${esc(ep.scope)}">迁移</button>`;
  return { toggle, del, mig };
}

function renderSkills(c) {
  const sec = settingsSectionEl();
  sec.innerHTML = `
    <h2>技能</h2>
    <div class="muted" style="margin-top:6px">加载中…</div>`;
  loadSkillState().catch((err) => {
    sec.innerHTML = `<h2>技能</h2>
      <p class="notice">加载技能失败：${esc(err.message)}</p>`;
  }).then(() => { if (settingsSec === "skills") skillRenderReady(); });
}

async function loadSkillState() {
  const res = await api("/api/config/skills");
  if (!res.ok) throw new Error(await res.text().catch(() => "加载失败"));
  const data = await res.json();
  skillState.skills = data.skills || [];
  skillState.groups = data.groups || [];
  skillState.scopes = data.scopes || [];
  skillState.enabled = !!data.enabled;
}

function skillRenderReady() {
  const sec = settingsSectionEl();
  sec.innerHTML = `
    <h2>技能</h2>
    <p class="intro">管理技能文件（项目根 / 用户根）。${
      skillState.enabled === false
        ? "当前 skill 能力未启用（config.yaml skill.enabled=false），仅管理目录；启用后这些技能会进入会话的技能目录注入。"
        : "启停/删除/添加/迁移立即写入磁盘，会话下一轮即生效（无重启）。"
    }</p>
    <div class="sk-toolbar">
      <div class="sk-search"><span class="sk-search-icon">🔍</span><input id="sk-search" placeholder="按名称搜索…" value="${esc(skillState.query)}"></div>
      <button class="sk-btn primary" id="sk-add">＋ 添加</button>
      <button class="sk-btn" id="sk-groups">分组</button>
    </div>
    <div class="sk-chipbar" id="sk-scopes"></div>
    <div class="sk-chipbar" id="sk-groupsbar"></div>
    <div id="sk-list"></div>`;
  renderSkillScopeChips();
  renderSkillGroupsBar();
  renderSkillList();

  $("sk-search").addEventListener("input", (e) => {
    skillState.query = e.target.value; renderSkillList();
  });
  $("sk-add").addEventListener("click", () => skillOpenAdd());
  $("sk-groups").addEventListener("click", () => skillOpenGroups());
}

async function skillRefresh() {
  await loadSkillState();
  skillRenderReady();
}

async function skillToggle(name, scope, on) {
  try {
    await skillAPI("set_enabled", { name, scope, enabled: on });
    await skillRefresh();
  } catch (err) { skillToast(err.message); }
}

async function skillDelete(name, scope) {
  if (!confirm(`确定删除技能「${name}」吗？此操作不可撤销。`)) return;
  try {
    await skillAPI("delete", { name, scope });
    await skillRefresh();
  } catch (err) { skillToast(err.message); }
}

function skillOpenMigrate(name, from) {
  const overlay = document.createElement("div");
  overlay.className = "sk-overlay";
  const scopeOpts = (skillState.scopes || []).map((s) =>
    `<label class="sk-groupcheck"><input type="radio" name="mig-to" value="${esc(s.id)}"${s.id !== from ? " checked" : ""}>${esc(s.label)}</label>`).join("");
  overlay.innerHTML = `<div class="sk-modal">
    <h3>迁移「${esc(name)}」</h3>
    <div class="sk-modalrow">目标作用域：</div>
    <div class="sk-modalrow">${scopeOpts}</div>
    <div class="sk-modalrow"><label class="sk-groupcheck"><input type="radio" name="mig-mode" value="move" checked>移动（源删除）</label></div>
    <div class="sk-modalrow"><label class="sk-groupcheck"><input type="radio" name="mig-mode" value="copy">复制（保留源）</label></div>
    <div class="sk-modalrow" style="justify-content:flex-end;gap:8px">
      <button class="sk-btn" data-sk-cancel>取消</button>
      <button class="sk-btn primary" data-sk-confirm>迁移</button>
    </div></div>`;
  document.body.appendChild(overlay);
  overlay.addEventListener("click", (ev) => {
    if (ev.target === overlay || ev.target.closest("[data-sk-cancel]")) { overlay.remove(); return; }
    if (ev.target.closest("[data-sk-confirm]")) {
      const to = overlay.querySelector('input[name="mig-to"]:checked')?.value;
      const mode = overlay.querySelector('input[name="mig-mode"]:checked')?.value;
      overlay.remove();
      skillAPI("migrate", { name, from, to, mode })
        .then(() => skillRefresh())
        .catch((err) => skillToast(err.message));
    }
  });
}

function skillOpenAdd() {
  const overlay = document.createElement("div");
  overlay.className = "sk-overlay";
  overlay.innerHTML = `<div class="sk-modal">
    <h3>添加技能</h3>
    <p class="intro">选择 .md 单文件、技能文件夹或 .zip 压缩包（也可拖拽到此区域）。</p>
    <div class="sk-drop" id="sk-drop" tabindex="0">点击选择文件 <span class="muted">或拖拽到此处</span></div>
    <div class="sk-modalrow">导入到作用域：</div>
    <div class="sk-modalrow">${(skillState.scopes || []).map((s) =>
      `<label class="sk-groupcheck"><input type="radio" name="add-scope" value="${esc(s.id)}"${s.id === "global" ? " checked" : ""}>${esc(s.label)}</label>`).join("")}</div>
    <div class="sk-modalrow" style="justify-content:flex-end;gap:8px">
      <span class="muted" id="sk-addfiles" style="margin-right:auto">未选择文件</span>
      <button class="sk-btn" data-sk-cancel>取消</button>
      <button class="sk-btn primary" data-sk-confirm>导入</button>
    </div>
    <input type="file" id="sk-fileinput" multiple hidden></div>`;
  document.body.appendChild(overlay);

  const dropZone = overlay.querySelector("#sk-drop");
  const fileInput = overlay.querySelector("#sk-fileinput");
  const filesLabel = overlay.querySelector("#sk-addfiles");
  let selected = [];
  dropZone.addEventListener("click", () => fileInput.click());
  fileInput.addEventListener("change", () => {
    selected = [...fileInput.files];
    filesLabel.textContent = selected.length ? `已选 ${selected.length} 个文件` : "未选择文件";
  });
  ["dragover", "dragenter"].forEach((ev) => dropZone.addEventListener(ev, (e) => { e.preventDefault(); dropZone.classList.add("dragging"); }));
  ["dragleave", "drop"].forEach((ev) => dropZone.addEventListener(ev, (e) => { e.preventDefault(); dropZone.classList.remove("dragging"); }));
  dropZone.addEventListener("drop", (e) => {
    selected = [...(e.dataTransfer.files || [])];
    filesLabel.textContent = selected.length ? `已选 ${selected.length} 个文件` : "未选择文件";
  });

  overlay.addEventListener("click", (ev) => {
    if (ev.target === overlay || ev.target.closest("[data-sk-cancel]")) { overlay.remove(); return; }
    if (ev.target.closest("[data-sk-confirm]")) {
      const scope = overlay.querySelector('input[name="add-scope"]:checked')?.value || "global";
      const kind = selected.length === 1 && /\.zip$/i.test(selected[0].name) ? "zip" : (selected.some((f) => f.webkitRelativePath || (f.name && f.name.includes("/"))) ? "bundle" : (selected.length ? "flat" : ""));
      if (!kind || selected.length === 0) { skillToast("请先选择文件"); return; }
      overlay.remove();
      skillAddFiles(kind, selected, scope);
    }
  });
}

async function skillAddFiles(kind, files, scope) {
  try {
    const uploads = [];
    for (const f of files) {
      // Bundle dirs come through File.path (webkitRelativePath); keep a flat-relative path.
      const path = (f.webkitRelativePath || f.name).replace(/\\/g, "/");
      const buf = await f.arrayBuffer();
      uploads.push({ path, base64: bufToBase64(buf) });
    }
    await skillAPI("add", { kind, files: uploads, scope });
    await skillRefresh();
  } catch (err) { skillToast(err.message); }
}

function bufToBase64(buf) {
  let bin = "";
  const bytes = new Uint8Array(buf);
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}

function skillOpenGroups() {
  const overlay = document.createElement("div");
  overlay.className = "sk-overlay";
  overlay.innerHTML = `<div class="sk-modal">
    <h3>技能分组</h3>
    <div class="sk-modalrow">
      <input id="sk-groupname" class="sk-search" style="height:36px;padding:0 12px" placeholder="分组名称…">
      <button class="sk-btn primary" id="sk-groupadd">新建</button>
    </div>
    <div id="sk-grouplist" style="margin-top:6px"></div>
    <div class="sk-modalrow" style="justify-content:flex-end"><button class="sk-btn" data-sk-close>关闭</button></div>
    <div class="sk-modalrow" id="sk-groupscope" hidden></div>
  </div>`;
  document.body.appendChild(overlay);

  const renderList = () => {
    const listEl = overlay.querySelector("#sk-grouplist");
    listEl.textContent = "";
    if (skillState.groups.length === 0) {
      listEl.innerHTML = `<div class="sk-empty">还没有分组</div>`;
      return;
    }
    for (const g of skillState.groups) {
      const row = document.createElement("div");
      row.className = "sk-grouprow";
      const count = Object.values(g.scopes || {}).reduce((s, arr) => s + arr.length, 0);
      row.innerHTML = `<span style="flex:1">${esc(g.name)} <span class="muted">(${count})</span></span>
        <button class="sk-btn" data-sk-groupedit="${esc(g.id)}">编辑</button>
        <button class="sk-btn danger" data-sk-groupdel="${esc(g.id)}">删除</button>`;
      listEl.appendChild(row);
    }
  };

  const openScope = (g) => {
    const scopeEl = overlay.querySelector("#sk-groupscope");
    scopeEl.hidden = false;
    const scope = skillState.scopes[0]?.id || "global";
    const names = skillState.skills.filter((s) => s.scope === scope).map((s) => s.name);
    const members = (g.scopes && g.scopes[scope]) || [];
    scopeEl.innerHTML = `<h3>编辑「${esc(g.name)}」（${esc(scope)} 作用域）</h3>
      <div style="display:flex;flex-direction:column;gap:6px;max-height:240px;overflow:auto">
        ${names.map((n) => {
          const on = members.includes(n);
          return `<label class="sk-groupcheck"><input type="checkbox" data-skill-check="${esc(n)}"${on ? " checked" : ""}>${esc(n)}</label>`;
        }).join("") || `<span class="muted">该作用域没有技能</span>`}
      </div>
      <div class="sk-modalrow" style="justify-content:flex-end;gap:8px">
        <button class="sk-btn" data-sk-scopecancel>取消</button>
        <button class="sk-btn primary" data-sk-scopesave>保存</button>
      </div>`;
    scopeEl.dataset.groupId = g.id;
    scopeEl.dataset.groupName = g.name;
  };

  overlay.addEventListener("click", async (ev) => {
    if (ev.target === overlay || ev.target.closest("[data-sk-close]")) { overlay.remove(); return; }
    if (ev.target.closest("#sk-groupadd")) {
      const name = overlay.querySelector("#sk-groupname").value.trim();
      if (!name) { skillToast("请输入分组名称"); return; }
      try { skillState.groups = (await skillAPI("group_save", { group_name: name, scope: "", names: [] })).groups || skillState.groups; renderList(); }
      catch (err) { skillToast(err.message); }
      return;
    }
    const del = ev.target.closest("[data-sk-groupdel]");
    if (del) {
      if (!confirm("删除此分组？")) return;
      try { skillState.groups = (await skillAPI("group_delete", { group_id: del.dataset.skGroupdel })).groups || skillState.groups; renderList(); }
      catch (err) { skillToast(err.message); }
      return;
    }
    const edit = ev.target.closest("[data-sk-groupedit]");
    if (edit) { openScope(skillState.groups.find((g) => g.id === edit.dataset.skGroupedit)); return; }
    if (ev.target.closest("[data-sk-scopecancel]")) { const se = overlay.querySelector("#sk-groupscope"); se.hidden = true; return; }
    if (ev.target.closest("[data-sk-scopesave]")) {
      const se = overlay.querySelector("#sk-groupscope");
      const gid = se.dataset.groupId, gname = se.dataset.groupName;
      const names = [...se.querySelectorAll('input[type="checkbox"]:checked')].map((el) => el.dataset.skillCheck);
      const scope = "global";
      try { skillState.groups = (await skillAPI("group_save", { group_id: gid, group_name: gname, scope, names })).groups || skillState.groups; se.hidden = true; renderList(); }
      catch (err) { skillToast(err.message); }
      return;
    }
  });
  renderList();
}

function skillToast(msg) {
  const el = document.createElement("div");
  el.className = "toast";
  el.textContent = msg;
  document.body.appendChild(el);
  requestAnimationFrame(() => el.classList.add("show"));
  setTimeout(() => { el.classList.remove("show"); setTimeout(() => el.remove(), 200); }, 2600);
}


function renderSkillScopeChips() {
  const bar = $("sk-scopes");
  const all = [{ id: "all", label: "全部" }].concat(skillState.scopes || []);
  bar.textContent = "";
  for (const s of all) {
    const n = s.id === "all" ? skillState.skills.length : skillState.skills.filter((x) => x.scope === s.id).length;
    const b = document.createElement("button");
    b.className = "sk-chip" + (skillState.scopeFilter === s.id ? " active" : "");
    b.dataset.scope = s.id;
    b.innerHTML = `${esc(s.label)}<span class="sk-chipcount">${n}</span>`;
    b.addEventListener("click", () => { skillState.scopeFilter = s.id; renderSkillScopeChips(); renderSkillList(); });
    bar.appendChild(b);
  }
}

function renderSkillGroupsBar() {
  const bar = $("sk-groupsbar");
  bar.textContent = "";
  const groups = [{ id: "", name: "全部" }].concat(skillState.groups || []);
  for (const g of groups) {
    const b = document.createElement("button");
    b.className = "sk-chip" + (skillState.groupFilter === g.id ? " active" : "");
    b.dataset.group = g.id;
    b.textContent = g.name;
    b.addEventListener("click", () => { skillState.groupFilter = g.id; renderSkillGroupsBar(); renderSkillList(); });
    bar.appendChild(b);
  }
}

function skillFiltered() {
  const q = skillState.query.trim().toLowerCase();
  return skillState.skills.filter((e) => {
    if (skillState.scopeFilter !== "all" && e.scope !== skillState.scopeFilter) return false;
    if (skillState.groupFilter) {
      const g = skillState.groups.find((x) => x.id === skillState.groupFilter);
      if (!g || !((g.scopes && g.scopes[e.scope]) || []).includes(e.name)) return false;
    }
    if (q && !e.name.toLowerCase().includes(q)) return false;
    return true;
  });
}

function renderSkillList() {
  const list = $("sk-list");
  const filtered = skillFiltered();
  if (filtered.length === 0) {
    list.innerHTML = `<div class="sk-empty">没有匹配的技能</div>`;
    return;
  }
  const grid = document.createElement("div");
  grid.className = "sk-grid";
  for (const e of filtered) {
    const { toggle, del, mig } = skillOpts(e);
    const card = document.createElement("div");
    card.className = "sk-card";
    card.dataset.skillName = e.name;
    card.dataset.skillScope = e.scope;
    const groups = skillGroupNames(e);
    card.innerHTML = `
      <div class="sk-cardhead">
        <span class="sk-status ${e.enabled ? "on" : "off"}"></span>
        <span class="sk-cardname">${esc(e.name)}</span>
        <span class="sk-tag ${e.enabled ? "" : "off"}">${e.enabled ? "已启用" : "已停用"}</span>
      </div>
      <div class="sk-carddesc">${esc(e.description || "")}</div>
      <div class="sk-cardmeta">${esc(skillScopeLabel(e.scope))} · ${esc(e.source || "")}${e.rel ? " · " + esc(e.rel) : ""}${groups.length ? " · 分组: " + esc(groups.join(", ")) : ""}</div>
      <div class="sk-actions">${toggle}${del}${mig}</div>`;
    card.addEventListener("click", async (ev) => {
      if (ev.target.closest("button")) return;
      const key = e.name + "@" + e.scope;
      const was = skillState.expanded === key;
      skillState.expanded = was ? null : key;
      const detail = card.querySelector(".sk-detail");
      if (detail) { detail.remove(); if (!was) return; }
      if (!was) await skillRenderDetail(card, e);
    });
    grid.appendChild(card);
  }
  list.textContent = "";
  list.appendChild(grid);
  grid.addEventListener("click", (ev) => {
    const t = ev.target.closest("[data-skill-toggle],[data-skill-delete],[data-skill-migrate]");
    if (!t) return;
    ev.stopPropagation();
    if (t.dataset.skillToggle !== undefined) skillToggle(t.dataset.skillToggle, t.dataset.skillScope, t.dataset.skillOn === "1");
    else if (t.dataset.skillDelete !== undefined) skillDelete(t.dataset.skillDelete, t.dataset.skillScope);
    else if (t.dataset.skillMigrate !== undefined) skillOpenMigrate(t.dataset.skillMigrate, t.dataset.skillScope);
  });
}

async function skillRenderDetail(card, e) {
  try {
    const res = await skillAPI("content", { name: e.name, scope: e.scope });
    const d = document.createElement("div");
    d.className = "sk-detail";
    d.innerHTML = `<pre>${esc(res.content || "")}</pre>`;
    card.appendChild(d);
  } catch (err) {
    const d = document.createElement("div");
    d.className = "sk-detail";
    d.innerHTML = `<pre style="color:var(--dsw-alias-state-error-primary)">${esc(err.message)}</pre>`;
    card.appendChild(d);
  }
}


function renderSettingsSec() {
  const c = settingsConfig;
  const sec = settingsSectionEl();
  sec.textContent = "";
  $("settings-sec-title").textContent = SETTINGS_SECTIONS.find((s) => s.id === settingsSec)?.label || "";
  if (!c) { sec.innerHTML = `<div class="muted">加载中…</div>`; return; }
  if (settingsSec === "general") renderGeneral(c);
  else if (settingsSec === "model") renderModel(c);
  else if (settingsSec === "caps") renderCaps(c);
  else if (settingsSec === "skills") renderSkills(c);
}

async function renderSettings() {
  const loading = $("settings-loading"), errEl = $("settings-error");
  loading.classList.remove("hidden");
  errEl.classList.add("hidden");
  try {
    const res = await api("/api/config");
    settingsConfig = await res.json();
    config = settingsConfig;
    loadConfigLabels();
    loading.classList.add("hidden");
    renderSettingsNav();
    renderSettingsSec();
  } catch (e) {
    loading.classList.add("hidden");
    errEl.textContent = "加载设置失败：" + e.message;
    errEl.classList.remove("hidden");
  }
}

// ---- routing --------------------------------------------------------------------
async function route() {
  const h = location.hash;
  workspaceEl.classList.toggle("hidden", !(h === "" || h === "#/" || h.startsWith("#/chat")));
  settingsEl.classList.toggle("hidden", h !== "#/settings");
  placeholderEl.classList.toggle("hidden", h !== "#/kb" && h !== "#/kb/");
  if (h === "#/settings") { renderSettings(); }
  else if (h === "#/kb" || h === "#/kb/") {
    $("ph-title").textContent = "知识库";
    $("ph-note").textContent = "KB 全量后挂（占位）。";
  }
}
window.addEventListener("hashchange", () => route());

// ---- P4: header actions (dsh ui-subagent catalog + ui-jobs list) ----------
// Two independent count triggers beside the session title, each with its own
// popover list; a trigger stays hidden until its session actually has entries
// (dsh: an ordinary conversation never grows a control for a capability it is
// not using). Copy strings follow the dsh locales (ui-jobs / ui-subagent).
// ----------------------------------------------------------------------------
const subsRoot = $("subs-root"), subsTrigger = $("subs-trigger"),
      subsCount = $("subs-count"), subsMenu = $("subs-menu");
const jobsRoot = $("jobs-root"), jobsTrigger = $("jobs-trigger"),
      jobsCount = $("jobs-count"), jobsMenu = $("jobs-menu");
let subsOpen = false, jobsOpen = false, runsPollTimer = null, runsClockTimer = null, runsBusy = false;

const JOB_STATUS_WORDS = {
  running: "运行中", stopping: "正在停止", completed: "已完成", killed: "已取消", failed: "已失败",
};
function jobDotState(status) {
  if (status === "running") return "running";
  if (status === "stopping" || status === "killed") return "warning";
  if (status === "failed") return "error";
  return "done"; // completed / unknown
}
// dsh ui-jobs formatDuration copy strings: {hours}小时{minutes}分,
// {minutes}分{seconds}秒 (seconds zero-padded), {seconds}秒.
function fmtDuration(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  if (h > 0) return `${h}小时${m}分`;
  if (m > 0) return `${m}分${String(sec).padStart(2, "0")}秒`;
  return `${sec}秒`;
}

function setMenuOpen(menu, trigger, open) {
  menu.classList.toggle("hidden", !open);
  trigger.setAttribute("aria-expanded", open ? "true" : "false");
}
function toggleSubs(force) {
  const next = force !== undefined ? force : !subsOpen;
  if (next === subsOpen) return;
  subsOpen = next;
  setMenuOpen(subsMenu, subsTrigger, subsOpen);
  if (subsOpen) { loadRuns(); startRunsTimers(); } else { stopRunsTimersIfIdle(); }
}
function toggleJobs(force) {
  const next = force !== undefined ? force : !jobsOpen;
  if (next === jobsOpen) return;
  jobsOpen = next;
  setMenuOpen(jobsMenu, jobsTrigger, jobsOpen);
  if (jobsOpen) { loadRuns(); startRunsTimers(); } else { stopRunsTimersIfIdle(); }
}
// Switching to the hero / a blank session closes both menus (dsh: header
// actions belong to an active session).
function syncHeaderActions() {
  if (!currentID) { toggleSubs(false); toggleJobs(false); }
}
function startRunsTimers() {
  stopRunsTimers();
  // 1s live duration clock (only matters while a job is live; cheap enough)
  runsClockTimer = setInterval(() => {
    document.querySelectorAll(".hd-duration[data-live]").forEach((el) => {
      const start = Number(el.dataset.start);
      if (start) el.textContent = fmtDuration(Date.now() - start);
    });
  }, 1000);
  // 10s list refresh; paused while both menus are closed
  runsPollTimer = setInterval(() => {
    if (document.visibilityState !== "hidden") loadRuns();
  }, 10000);
}
function stopRunsTimers() {
  if (runsClockTimer) { clearInterval(runsClockTimer); runsClockTimer = null; }
  if (runsPollTimer) { clearInterval(runsPollTimer); runsPollTimer = null; }
}
function stopRunsTimersIfIdle() {
  if (!subsOpen && !jobsOpen) stopRunsTimers();
}

// orderedJobs mirrors dsh ui-jobs ordered(): live (running/stopping) first by
// startedAt ascending, then settled by finishedAt descending.
function orderedJobs(jobs) {
  const live = [], settled = [];
  for (const j of jobs) {
    if (j.status === "running" || j.status === "stopping") live.push(j);
    else settled.push(j);
  }
  live.sort((a, b) => new Date(a.started_at) - new Date(b.started_at));
  settled.sort((a, b) => new Date(b.finished_at) - new Date(a.finished_at));
  return live.concat(settled);
}

// renderSubagents fills the subagent menu and the trigger's count/visibility
// (dsh SubagentCatalogAction: count label "N 个子代理" / "N 个子代理，正在运行"
// with an ongoing dot; the action disappears when the session has none).
function renderSubagents(list) {
  const arr = Array.isArray(list) ? list : [];
  subsMenu.textContent = "";
  const runningCount = arr.filter((s) => s.running).length;
  subsTrigger.classList.toggle("hidden", arr.length === 0);
  const dot = subsTrigger.querySelector(".hd-dot");
  if (dot) dot.dataset.state = runningCount > 0 ? "running" : "idle";
  subsCount.textContent = runningCount > 0
    ? `${runningCount} 个子代理，正在运行`
    : `${arr.length} 个子代理`;
  subsTrigger.setAttribute("aria-label", subsCount.textContent);
  if (arr.length === 0) {
    subsMenu.innerHTML = `<div class="hd-empty">暂无子代理</div>`;
    return;
  }
  const rows = [...arr].sort((a, b) => (b.running ? 1 : 0) - (a.running ? 1 : 0));
  for (const s of rows) {
    const row = document.createElement("div");
    row.className = "hd-row";
    row.innerHTML = `
      <span class="p4-dot" data-state="${s.running ? "running" : "done"}"></span>
      <span class="hd-label" title="${esc(s.id || "")}">${esc(s.label || s.id || "")}</span>
      <span class="hd-status">${s.running ? "正在运行" : "当前未运行"}</span>`;
    subsMenu.appendChild(row);
  }
}

// renderJobs fills the jobs menu and the trigger's count/visibility (dsh
// JobListAction: count label "N 个后台任务" / "N 个后台任务运行中"; rows are
// dot + kind badge + label + status/detail + duration, settled rows dimmed).
function renderJobs(list) {
  const arr = Array.isArray(list) ? list : [];
  jobsMenu.textContent = "";
  const liveCount = arr.filter((j) => j.status === "running" || j.status === "stopping").length;
  jobsTrigger.classList.toggle("hidden", arr.length === 0);
  const dot = jobsTrigger.querySelector(".hd-dot");
  if (dot) dot.dataset.state = liveCount > 0 ? "running" : "idle";
  jobsCount.textContent = liveCount > 0
    ? `${liveCount} 个后台任务运行中`
    : `${arr.length} 个后台任务`;
  jobsTrigger.setAttribute("aria-label", jobsCount.textContent);
  if (arr.length === 0) {
    jobsMenu.innerHTML = `<div class="hd-empty">暂无后台任务</div>`;
    return;
  }
  for (const j of orderedJobs(arr)) {
    const isLive = j.status === "running" || j.status === "stopping";
    const start = new Date(j.started_at).getTime();
    const dur = isLive
      ? (start ? fmtDuration(Date.now() - start) : "")
      : (j.finished_at ? fmtDuration(new Date(j.finished_at) - start) : "");
    const row = document.createElement("div");
    row.className = "hd-row" + (isLive ? "" : " settled");
    row.innerHTML = `
      <span class="p4-dot" data-state="${jobDotState(j.status)}"></span>
      ${j.kind ? `<span class="hd-kind">${esc(j.kind)}</span>` : ""}
      <span class="hd-label" title="${esc(j.label || j.id || "")}">${esc(j.label || j.id || "")}</span>
      <span class="hd-status" title="${esc(j.detail || "")}">${esc(j.detail || JOB_STATUS_WORDS[j.status] || j.status)}</span>
      <span class="hd-duration"${isLive && start ? ` data-live data-start="${start}"` : ""}>${dur}</span>`;
    jobsMenu.appendChild(row);
  }
}

async function loadRuns() {
  if (runsBusy) return;
  runsBusy = true;
  try {
    const q = currentID ? `?session_id=${encodeURIComponent(currentID)}` : "";
    const [subsRes, jobsRes] = await Promise.all([api("/api/subagents" + q), api("/api/jobs" + q)]);
    const subs = subsRes.status === 501 ? [] : await subsRes.json();
    const jobs = jobsRes.status === 501 ? [] : await jobsRes.json();
    renderSubagents(subs);
    renderJobs(jobs);
  } catch (e) {
    if (e.message === "unauthorized") { toggleSubs(false); toggleJobs(false); return; }
    const msg = e.message || "未知错误";
    if (subsOpen && !subsTrigger.dataset.loaded) {
      subsMenu.innerHTML = `<div class="hd-empty">加载失败：${esc(msg)}<button class="hd-retry">重试</button></div>`;
      subsMenu.querySelector(".hd-retry").addEventListener("click", () => loadRuns());
    }
    if (jobsOpen && !jobsTrigger.dataset.loaded) {
      jobsMenu.innerHTML = `<div class="hd-empty">加载失败：${esc(msg)}<button class="hd-retry">重试</button></div>`;
      jobsMenu.querySelector(".hd-retry").addEventListener("click", () => loadRuns());
    }
  } finally {
    runsBusy = false;
  }
}

subsTrigger.addEventListener("click", (e) => { e.stopPropagation(); toggleSubs(); });
jobsTrigger.addEventListener("click", (e) => { e.stopPropagation(); toggleJobs(); });
subsMenu.addEventListener("click", (e) => e.stopPropagation());
jobsMenu.addEventListener("click", (e) => e.stopPropagation());
document.addEventListener("click", (e) => {
  if (!e.target.closest("#subs-root")) toggleSubs(false);
  if (!e.target.closest("#jobs-root")) toggleJobs(false);
});
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "hidden") stopRunsTimers();
  else if (subsOpen || jobsOpen) startRunsTimers();
});

// ---- boot ------------------------------------------------------------------------
function boot() {
  injectIcons();
  hideLogin();
  workspaceEl.classList.remove("hidden");
  setupDrag();
  setupNarrow();
  renderColumns();
  syncSidebarToggle();
  initThemeSystem();
  applyTheme();
  syncGrow();
  updatePlaceholder();
  syncPermSelect();
  syncSendButton();
  loadConfig();
  loadComposerPrefs();
  loadSessions();
  if (currentID) openSession(currentID);
  else {
    heroEl.classList.remove("hidden");
    setHeroPhase();
    loadWorkspaces().then(() => { syncHeroChip(); syncHeroPickState(); });
    syncHeroPickState();
  }
  pollTimer = setInterval(() => loadSessions(), 30000);
  route();
}

loginForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const t = $("tok").value.trim();
  if (!t) return;
  localStorage.setItem(KEY_TOKEN, t);
  loginMsg.classList.add("hidden");
  await boot();
});
newSessionBtn.addEventListener("click", () => newSession());
// Brand button: a New-Session shortcut while wide, an expand affordance in the
// rail (dsh SidebarRoot brand). The panel toggle folds/expands the column.
$("brand").addEventListener("click", () => {
  if (sidebarCollapsed()) toggleSidebar();
  else newSession();
});
$("sidebar-toggle").addEventListener("click", toggleSidebar);
$("settings-link").addEventListener("click", () => location.hash = "#/settings");
$("theme-toggle").addEventListener("click", toggleTheme);
$("theme-toggle-settings").addEventListener("click", toggleTheme);
$("settings-back").addEventListener("click", () => location.hash = "#/chat");
$("settings-close").addEventListener("click", () => location.hash = "#/chat");
$("back").addEventListener("click", () => location.hash = "#/chat");

// New-session hero workspace chip: opens the picker popover (dsh WorkspaceChip).
if (heroWsChip) heroWsChip.addEventListener("click", (e) => {
  e.stopPropagation();
  renderHeroMenu();
  syncHeroMenuPosition();
  toggleHeroMenu();
});
// Hero picker popover: a click anywhere outside closes it.
document.addEventListener("click", (e) => {
  if (heroMenuOpen && !e.target.closest("#hero-ws-chip, #hero-ws-menu")) toggleHeroMenu(false);
  if (heroModeOpen && !e.target.closest("#hero-mode-chip, #hero-mode-menu")) toggleModeMenu(false);
  if (cmdMenuOpen && !e.target.closest("#cmd-btn, #cmd-menu")) toggleCmdMenu(false);
  if (slashMenuOpen && !e.target.closest("#composer, #slash-menu")) closeSlashMenu();
  if (permMenuOpen && !e.target.closest("#perm-seat, #perm-menu")) togglePermMenu(false);
  if (modelMenuOpen && !e.target.closest("#model-seat, #model-menu")) toggleModelMenu(false);
});
// Hero mode (agent preset) chip: opens the mode menu (dsh AgentPresetSeat).
if (heroModeChip) heroModeChip.addEventListener("click", (e) => {
  e.stopPropagation();
  renderModeMenu();
  syncModeMenuPosition();
  toggleModeMenu();
});
// Composer command(＋) button: opens the minimal session-action menu.
if (cmdBtn) cmdBtn.addEventListener("click", (e) => {
  e.stopPropagation();
  renderCmdMenu();
  // Show before measuring so the real height positions the menu above the
  // bottom-edge button.
  toggleCmdMenu();
  syncCmdMenuPosition();
});
// Permission seat → dsh Access chip menu (shield glyph + label + chevron).
if (permSeat) permSeat.addEventListener("click", (e) => {
  e.stopPropagation();
  const open = !permMenuOpen;
  if (open) {
    renderPermMenu();
    togglePermMenu(true);
    syncPermSeatPosition();
    return;
  }
  togglePermMenu(false);
});
// Model seat button → two-level model/effort picker (dsh ModelSelect). Every
// open refreshes the catalog (dsh show() reloads the directory), so models
// saved in settings appear without a page reload.
if (modelSeat) modelSeat.addEventListener("click", (e) => {
  e.stopPropagation();
  const open = !modelMenuOpen;
  if (open) {
    modelPane = "root";
    renderModelMenu("");
    // Show before measuring so the real pane height positions the menu above
    // the bottom-edge seat (dsh ModelSelect opens upward).
    toggleModelMenu(true);
    syncModelSeatPosition();
    void refreshModelCatalog();
    return;
  }
  toggleModelMenu(false);
});
// dsh ModelSelect keyboard + blur: Escape backs a drilled pane out to the root
// first, ArrowDown/Up move focus among the items, and leaving the menu closes
// it (focus returning to the seat keeps it open).
if (modelMenu) {
  modelMenu.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      e.stopPropagation();
      if (modelPane !== "root") { modelPane = "root"; renderModelMenu(""); syncModelSeatPosition(); }
      else toggleModelMenu(false);
      return;
    }
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      if (e.target && e.target.id === "model-search") return; // the input handles its own keys
      const items = Array.from(modelMenu.querySelectorAll(".hm-item:not([disabled])"));
      if (!items.length) return;
      const cur = items.indexOf(document.activeElement);
      const next = items[(Math.max(cur, 0) + (e.key === "ArrowDown" ? 1 : -1) + items.length) % items.length];
      next.focus();
      e.preventDefault();
    }
  });
  modelMenu.addEventListener("focusout", (e) => {
    if (modelBusy) return; // a selection in flight re-renders the items; keep the menu
    const t = e.relatedTarget;
    if (t) {
      if (modelMenu.contains(t) || t === modelSeat) return;
      toggleModelMenu(false);
      return;
    }
    // relatedTarget null: the focused element was removed (pane-switch
    // re-render) or the user clicked a non-focusable spot. Defer and re-check
    // whether focus re-entered the menu (a pane switch re-focuses an item).
    setTimeout(() => {
      if (!modelMenuOpen) return;
      if (document.activeElement && modelMenu.contains(document.activeElement)) return;
      toggleModelMenu(false);
    }, 0);
  });
}
// Cmd/Ctrl+K focuses the composer draft; Escape closes any open composer popover.
document.addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && (e.key === "k" || e.key === "K")) {
    e.preventDefault();
    if (composerText) composerText.focus();
    return;
  }
  if (e.key === "Escape") {
    if (slashMenuOpen) closeSlashMenu();
    if (modelMenuOpen) toggleModelMenu(false);
    if (permMenuOpen) togglePermMenu(false);
    if (cmdMenuOpen) toggleCmdMenu(false);
    if (heroMenuOpen) toggleHeroMenu(false);
    if (heroModeOpen) toggleModeMenu(false);
    if (cmOpen) toggleCMPanel(false);
  }
});
window.addEventListener("resize", () => { if (slashMenuOpen) syncSlashMenuPosition(); });
window.addEventListener("scroll", () => { if (slashMenuOpen) syncSlashMenuPosition(); }, true);
// dsh ContextMeter: a pointer down outside the meter closes its open panel.
document.addEventListener("pointerdown", (e) => {
  if (cmOpen && contextMeter && !contextMeter.contains(e.target)) toggleCMPanel(false);
});
// Right details panel close button (dsh DetailsPanel close).
if (detailsCloseBtn) detailsCloseBtn.addEventListener("click", closeDetails);

boot();
