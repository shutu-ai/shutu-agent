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
let runningNode = null;         // "Deep diving..." element
let pollTimer = null;           // session-list refresh
let config = {};                // cached GET /api/config view

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
  if (logo) logo.src = dark ? "/static/logo_w.png" : "/static/logo_b.png";
  const tlogo = $("toggle-logo");
  if (tlogo) tlogo.src = dark ? "/static/logo_w.png" : "/static/logo_b.png";
  const icon = $("theme-toggle");
  if (icon) icon.textContent = dark ? "☀️" : "🌙";
  const icon2 = $("theme-toggle-settings");
  if (icon2) icon2.textContent = dark ? "☀️" : "🌙";
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
function renderMarkdown(text) {
  const t = esc(text);
  const blocks = [];
  let buf = "";
  // crude fenced code split (keeps pre blocks intact)
  const parts = t.split(/(```[\s\S]*?```)/g);
  for (const p of parts) {
    if (/^```/.test(p)) {
      const code = p.replace(/^```[^\n]*\n?/, "").replace(/```$/, "");
      blocks.push(`<pre><code>${code}</code></pre>`);
    } else if (p) {
      let s = p.replace(/`([^`\n]+)`/g, "<code>$1</code>");
      s = s.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
      s = s.replace(/(^|\n)#{1,4} ([^\n]+)/g, "$1<h3>$2</h3>");
      s = s.replace(/(^|\n)[-*] ([^\n]+)/g, "$1<li>$2</li>");
      s = s.replace(/(^|\n)\d+\. ([^\n]+)/g, "$1<li>$2</li>");
      if (/\n<li>/.test(s)) {
        s = s.replace(/<li>([\s\S]*?)$/, "<ul><li>$1</ul>");
      }
      const paras = s.split(/\n{2,}/).filter((x) => x.trim());
      blocks.push(paras.map((x) => `<p>${x.trim()}</p>`).join(""));
    }
  }
  buf = blocks.join("");
  return buf || esc(text);
}

// ---- message stream --------------------------------------------------------
// addUserMsg renders a user bubble; images (P5) is an optional list of
// {src, id} thumbnails shown above the text.
function addUserMsg(text, timeIso, images) {
  const inner = msgInner();
  const node = document.createElement("div");
  node.className = "msg user";
  let imgs = "";
  if (images && images.length) {
    const cls = images.length === 1 ? "single" : "multi";
    imgs = `<div class="msg-images ${cls}">${images.map((im) => `<img class="msg-image" src="${esc(im.src)}" alt="图片" loading="lazy">`).join("")}</div>`;
  }
  node.innerHTML = `<div class="msg-time">${fmtTime(timeIso)}</div>${imgs}<div class="bubble">${esc(text)}</div>`;
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

function addRunning() {
  if (runningNode) return;
  const inner = msgInner();
  runningNode = document.createElement("div");
  runningNode.className = "msg running";
  runningNode.innerHTML = `<div class="running-text">Deep diving...</div>`;
  inner.appendChild(runningNode);
  scrollToBottom(true);
}
function removeRunning() {
  if (runningNode) { runningNode.remove(); runningNode = null; }
}

function addReasoning(reasoningText, timeIso) {
  const inner = msgInner();
  const node = document.createElement("div");
  node.className = "msg reasoning";
  const summary = reasoningText.length > 60 ? reasoningText.slice(0, 60) + "…" : reasoningText;
  node.innerHTML = `
    <div class="reasoning-row"><span class="reasoning-caret">▶</span>💭 思考过程
      <span class="reasoning-summary">${esc(summary)}</span></div>
    <div class="reasoning-body">${esc(reasoningText)}</div>`;
  node.querySelector(".reasoning-row").addEventListener("click", () => {
    node.classList.toggle("open");
    scrollToBottom();
  });
  inner.appendChild(node);
}

function addAssistant(text, timeIso, seq) {
  const inner = msgInner();
  const node = document.createElement("div");
  node.className = "msg assistant";
  const fb = seq != null
    ? `<button class="act-btn" data-act="up" title="好">👍</button>
       <button class="act-btn" data-act="down" title="差">👎</button>`
    : "";
  node.innerHTML = `
    <div class="markdown">${text ? renderMarkdown(text) : "<p></p>"}</div>
    <div class="actions-row">
      <button class="act-btn" data-act="copy" title="复制">⧉</button>
      ${fb}
      <span class="act-time">${fmtTime(timeIso)}</span>
    </div>`;
  node.querySelector('[data-act="copy"]').addEventListener("click", () => {
    navigator.clipboard?.writeText(text || "").catch(() => {});
  });
  if (seq != null) {
    // P5 feedback: localStorage per (session, seq) — optimistic, no backend.
    const k = `pa_fb:${currentID || ""}:${seq}`;
    const upBtn = node.querySelector('[data-act="up"]');
    const downBtn = node.querySelector('[data-act="down"]');
    const cur = localStorage.getItem(k);
    if (cur === "up") upBtn.classList.add("active-up");
    if (cur === "down") downBtn.classList.add("active-down");
    const set = (val) => {
      const next = localStorage.getItem(k) === val ? "" : val;
      if (next) localStorage.setItem(k, next); else localStorage.removeItem(k);
      upBtn.classList.toggle("active-up", next === "up");
      downBtn.classList.toggle("active-down", next === "down");
    };
    upBtn.addEventListener("click", () => set("up"));
    downBtn.addEventListener("click", () => set("down"));
  }
  inner.appendChild(node);
  return node.querySelector(".markdown");
}

// appendAssistantStreaming: mutate the live assistant bubble with chunk text.
function appendAssistantStreaming(chunk, seq) {
  let md = streamState && streamState.node;
  if (!md) {
    removeRunning();
    const node = addAssistant("", null, seq);
    streamState = { node };
  }
  streamState.node.append(esc(chunk));
  scrollToBottom(true);
}
function finishAssistant(text, timeIso, seq) {
  removeRunning();
  if (streamState && streamState.node) {
    // replace accumulated DOM text with the final rendered markdown
    streamState.node.innerHTML = text ? renderMarkdown(text) : "<p></p>";
    streamState = null;
  } else if (text) {
    // replay path (snapshot with no streaming chunks): render the bubble fresh
    addAssistant(text, timeIso, seq);
  }
  if (streamActive) { streamActive = false; turnRunning = false; syncSendButton(); loadSessions(); refreshContextMeter(); }
  scrollToBottom(true);
}

function addToolEvent(ev) {
  const inner = msgInner();
  const node = document.createElement("div");
  const isErr = ev.type === "tool/error";
  node.className = "msg tool" + (isErr ? " error" : "");
  const body = isErr ? ev.summary : ev.tool_output || ev.summary || "（无输出）";
  const seq = ev.seq == null ? "" : String(ev.seq);
  node.dataset.seq = seq;
  toolMeta[seq] = { name: ev.tool_name || "Tool call", args: ev.tool_args || "", output: body, error: isErr };
  node.innerHTML = `
    <div class="tool-card">
      <span class="tool-icon">🔧</span>
      <span class="tool-title">${esc(ev.tool_name || "Tool call")}</span>
      <span class="tool-summary">${esc(ev.summary || "")}</span>
      <span class="tool-status-${isErr ? "err" : "ok"}">${isErr ? "✕" : "✓"}</span>
      <span class="tool-caret">▶</span>
    </div>
    <div class="tool-body${isErr ? " error" : ""}">${esc(body)}</div>`;
  node.querySelector(".tool-card").addEventListener("click", () => {
    node.classList.add("open");
    openDetails(seq);
    scrollToBottom();
  });
  inner.appendChild(node);
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
  card.innerHTML = `
    <div class="shv-title">${esc(g.title)}</div>
    ${timeHtml}`;
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
      await api("/api/workspaces", { method: "POST", body: JSON.stringify({ title }) });
      groupBy = "workspace";
      localStorage.setItem("pa_groupby", "workspace");
    }
    closeWsDialog();
    loadSessions();
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
}
$("ws-add").addEventListener("click", () => $("ws-folder").click());
// dsh add-workspace opens a folder dialog; the selected folder's name becomes
// the new workspace title (the workspace itself stays a grouping bucket in the
// local store — no directory is bound yet).
$("ws-folder").addEventListener("change", async () => {
  const f = $("ws-folder").files && $("ws-folder").files[0];
  if (f && f.webkitRelativePath) {
    const folderName = f.webkitRelativePath.split("/")[0];
    try {
      await api("/api/workspaces", { method: "POST", body: JSON.stringify({ title: folderName }) });
      groupBy = "workspace";
      localStorage.setItem("pa_groupby", "workspace");
    } catch (e) { if (e.message !== "unauthorized") console.error(e); }
  }
  $("ws-folder").value = "";
  loadSessions();
});
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
    runningNode = null;
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
  runningNode = null;
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
  syncRunsTrigger();
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
  const rt = config.mode || "";
  modeBadgeEl.textContent = MODE_DISPLAY[rt] || rt;
  modeBadgeEl.classList.toggle("hidden", !modeBadgeEl.textContent);
  modeBadgeEl.classList.remove("mode-minimal", "mode-code");
  if (rt === "minimal") modeBadgeEl.classList.add("mode-minimal");
  if (rt === "code") modeBadgeEl.classList.add("mode-code");
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
// turn is in flight it shows ■ and aborts; otherwise it is the send arrow.
function syncSendButton() {
  if (!sendBtn) return;
  sendBtn.classList.toggle("running", turnRunning);
  sendBtn.textContent = turnRunning ? "■" : "➤";
  sendBtn.title = turnRunning ? "停止" : "发送";
  sendBtn.setAttribute("aria-label", turnRunning ? "停止" : "发送");
}
async function stopTurn() {
  if (!currentID) return;
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
  runningNode = null;
  streamActive = false;
  turnRunning = false;
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
  syncRunsTrigger();
  if (!id) { sessionCfg = { model: "", permission: "" }; setHeroPhase(); if (contextMeter) { contextMeter.textContent = ""; cmOpen = false; } return; }
  loadSessionConfig(id);
  return Promise.all([loadEvents(id), connectStream(id)]);
}

async function loadEvents(id) {
  try {
    const res = await api(`/api/sessions/${encodeURIComponent(id)}/events`);
    const evs = await res.json();
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
    refreshContextMeter();
    scrollToBottom(true);
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
}

function renderEvent(ev, replay) {
  // First event of an empty session: the turn has begun, so leave the centered
  // hero and dock the composer (dsh: 第一次输入提交后输入条下移).
  if (sessionEmpty) { sessionEmpty = false; setHeroPhase(); }
  switch (ev.type) {
    case "user/message": {
      const imgs = (ev.images || []).map((iv) => ({
        src: `/api/sessions/${encodeURIComponent(currentID)}/attachments/${iv.id}`,
        id: iv.id,
      }));
      addUserMsg(ev.summary || "", ev.time, imgs.length ? imgs : null);
      break;
    }
    case "assistant/message":
      if (ev.reasoning) addReasoning(ev.reasoning, ev.time);
      finishAssistant(ev.summary || "", ev.time, ev.Seq);
      break;
    case "tool/result":
    case "tool/error":
      if (ev.type === "tool/error" && !ev.tool_name) addErrorRow(ev);
      else addToolEvent(ev);
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
  if (ev.type === "assistant/chunk") {
    appendAssistantStreaming(ev.summary || "", ev.Seq);
    return;
  }
  renderEvent(ev, false);
  if (ev.type === "assistant/message") { streamState = null; }
}

// ---- composer ---------------------------------------------------------------
function syncGrow() {
  // The hidden ::after replica layer (grow-wrap) sizes the textarea; it reads
  // the value through the data attribute so only one live source exists.
  growWrapEl.dataset.replicatedValue = composerText.value + "\n";
}
function setComposerDisabled(disabled) {
  composerBox.classList.toggle("disabled", disabled);
  sendBtn.disabled = disabled;
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
});

async function sendMessage() {
  const text = composerText.value.trim();
  if ((!text && drafts.length === 0) || !currentID) return;
  setComposerDisabled(true);
  try {
    addUserMsg(text, new Date().toISOString(), drafts.length ? drafts.map((d) => ({ src: d.url })) : null);
    // Submitting the first message moves the composer from the centered hero
    // down to the docked slot (dsh: 第一次输入提交后输入条下移).
    if (sessionEmpty) { sessionEmpty = false; setHeroPhase(); }
    addRunning();
    streamActive = true;
    turnRunning = true;
    syncSendButton();
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
    const res = await api(`/api/sessions/${encodeURIComponent(currentID)}/message`, {
      method: "POST",
      body: JSON.stringify({ text, images }),
    });
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      throw new Error(body.error || ("HTTP " + res.status));
    }
  } catch (e) {
    if (e.message !== "unauthorized") {
      removeRunning();
      addErrorRow({ summary: e.message });
      console.error(e);
    }
  } finally {
    setComposerDisabled(false);
    // The POST settles exactly when the turn settles (success or error): a
    // failed turn produces no assistant/message event, so finishAssistant
    // never ran — reset the run state and refresh the ContextMeter here.
    if (streamActive) { streamActive = false; turnRunning = false; syncSendButton(); }
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
    // / WSL). Any choice except "关闭" enables the persistent terminal (M9).
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

// ---- P4: runs panel (subagents + background jobs, dsh ui-subagent / ui-jobs)
// The sidebar shortcut was removed on user request; the panel logic stays for
// a future entry (runs-tab is gone, so the panel is currently unreachable).
// ----------------------------------------------------------------------------
const runsPanel = $("runs-panel"), runsTab = $("runs-tab"),
      runsSubs = $("runs-subs"), runsJobs = $("runs-jobs"), runsRefresh = $("runs-refresh");
let runsOpen = false, runsPollTimer = null, runsClockTimer = null, runsBusy = false;

const JOB_STATUS_WORDS = {
  running: "运行中", stopping: "正在停止", completed: "已完成", killed: "已取消", failed: "已失败",
};
function jobDotState(status) {
  if (status === "running") return "running";
  if (status === "stopping" || status === "killed") return "warning";
  if (status === "failed") return "error";
  return "done"; // completed / unknown
}
function fmtDuration(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  if (h > 0) return `${h}小时${m}分`;
  if (m > 0) return `${m}分${sec}秒`;
  return `${sec}秒`;
}

function toggleRuns(force) {
  const next = force !== undefined ? force : !runsOpen;
  if (next === runsOpen) return;
  runsOpen = next;
  runsPanel.classList.toggle("hidden", !runsOpen);
  if (runsTab) {
    runsTab.classList.toggle("active", runsOpen);
    runsTab.setAttribute("aria-expanded", runsOpen ? "true" : "false");
  }
  if (runsOpen) {
    loadRuns();
    startRunsTimers();
    positionRunsPop();
  } else {
    stopRunsTimers();
  }
}
// Anchor the popover under its trigger (dsh popover menus hang from the trigger).
function positionRunsPop() {
  const tr = runsTab, pop = runsPanel;
  if (!tr || !pop) return;
  const r = tr.getBoundingClientRect();
  let left = r.right - 260;
  if (left < 8) left = 8;
  pop.style.left = left + "px";
  pop.style.top = (r.bottom + 6) + "px";
}
// The runs trigger only appears for an active session (dsh session-header
// actions); switching to the hero / a blank session hides and closes it.
function syncRunsTrigger() {
  if (!runsTab) return;
  const active = !!currentID;
  runsTab.classList.toggle("hidden", !active);
  if (!active) toggleRuns(false);
}
function startRunsTimers() {
  stopRunsTimers();
  // 1s live duration clock (only matters while a job is live; cheap enough)
  runsClockTimer = setInterval(() => {
    document.querySelectorAll(".run-duration[data-live]").forEach((el) => {
      const start = Number(el.dataset.start);
      if (start) el.textContent = fmtDuration(Date.now() - start);
    });
  }, 1000);
  // 10s list refresh; paused while the tab is hidden
  runsPollTimer = setInterval(() => {
    if (document.visibilityState !== "hidden") loadRuns();
  }, 10000);
}
function stopRunsTimers() {
  if (runsClockTimer) { clearInterval(runsClockTimer); runsClockTimer = null; }
  if (runsPollTimer) { clearInterval(runsPollTimer); runsPollTimer = null; }
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

function renderSubagents(list) {
  if (!Array.isArray(list)) return;
  runsSubs.textContent = "";
  if (list.length === 0) {
    runsSubs.innerHTML = `<div class="runs-empty">暂无子代理</div>`;
    return;
  }
  const rows = [...list].sort((a, b) => (b.running ? 1 : 0) - (a.running ? 1 : 0));
  for (const s of rows) {
    const row = document.createElement("div");
    row.className = "run-row";
    const state = s.running ? "running" : "done";
    row.innerHTML = `
      <span class="p4-dot" data-state="${state}"></span>
      <span class="run-label" title="${esc(s.id || "")}">${esc(s.label || s.id || "")}</span>
      <span class="run-sub">${s.running ? "正在运行" : "当前未运行"}</span>`;
    runsSubs.appendChild(row);
  }
}

function renderJobs(list) {
  if (!Array.isArray(list)) return;
  runsJobs.textContent = "";
  if (list.length === 0) {
    runsJobs.innerHTML = `<div class="runs-empty">暂无后台任务</div>`;
    return;
  }
  for (const j of orderedJobs(list)) {
    const isLive = j.status === "running" || j.status === "stopping";
    const start = new Date(j.started_at).getTime();
    const dur = isLive
      ? (start ? fmtDuration(Date.now() - start) : "")
      : (j.finished_at ? fmtDuration(new Date(j.finished_at) - start) : "");
    const row = document.createElement("div");
    row.className = "run-row" + (isLive ? "" : " settled");
    row.innerHTML = `
      <span class="p4-dot" data-state="${jobDotState(j.status)}"></span>
      ${j.kind ? `<span class="run-kind">${esc(j.kind)}</span>` : ""}
      <span class="run-label" title="${esc(j.label || j.id || "")}">${esc(j.label || j.id || "")}</span>
      <span class="run-sub" title="${esc(j.detail || "")}">${esc(j.detail || JOB_STATUS_WORDS[j.status] || j.status)}</span>
      <span class="run-duration"${isLive && start ? ` data-live data-start="${start}"` : ""}>${dur}</span>`;
    runsJobs.appendChild(row);
  }
}

async function loadRuns() {
  if (runsBusy) return;
  runsBusy = true;
  runsRefresh.classList.add("spinning");
  // Show loading placeholders on the first open only.
  if (!runsSubs.dataset.loaded) { runsSubs.innerHTML = `<div class="runs-loading">正在加载子代理…</div>`; runsJobs.innerHTML = `<div class="runs-loading">正在加载任务…</div>`; }
  try {
    const q = currentID ? `?session_id=${encodeURIComponent(currentID)}` : "";
    const [subsRes, jobsRes] = await Promise.all([api("/api/subagents" + q), api("/api/jobs" + q)]);
    const subs = subsRes.status === 501 ? [] : await subsRes.json();
    const jobs = jobsRes.status === 501 ? [] : await jobsRes.json();
    runsSubs.dataset.loaded = "1"; runsJobs.dataset.loaded = "1";
    renderSubagents(subs);
    renderJobs(jobs);
    // Reflect a running subagent/job on the header trigger dot (dsh StateDot).
    if (runsTab) {
      const dot = runsTab.querySelector(".ht-dot");
      const anyRunning = (Array.isArray(subs) && subs.some((s) => s.running))
        || (Array.isArray(jobs) && jobs.some((j) => j.status === "running" || j.status === "stopping"));
      if (dot) dot.dataset.state = anyRunning ? "running" : "idle";
    }
  } catch (e) {
    if (e.message === "unauthorized") { toggleRuns(false); }
    const msg = e.message || "未知错误";
    if (!runsSubs.dataset.loaded) {
      runsSubs.innerHTML = `<div class="runs-error">加载失败：${esc(msg)}<button class="runs-retry">重试</button></div>`;
      runsSubs.querySelector(".runs-retry").addEventListener("click", () => loadRuns());
    }
    if (!runsJobs.dataset.loaded) {
      runsJobs.innerHTML = `<div class="runs-error">加载失败：${esc(msg)}<button class="runs-retry">重试</button></div>`;
      runsJobs.querySelector(".runs-retry").addEventListener("click", () => loadRuns());
    }
  } finally {
    runsBusy = false;
    runsRefresh.classList.remove("spinning");
  }
}

if (runsTab) runsTab.addEventListener("click", (e) => { e.stopPropagation(); toggleRuns(); });
runsRefresh.addEventListener("click", (e) => { e.stopPropagation(); loadRuns(); });
runsPanel.addEventListener("click", (e) => e.stopPropagation());
document.addEventListener("click", (e) => { if (!e.target.closest("#runs-panel, #runs-tab")) toggleRuns(false); });document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "hidden") stopRunsTimers();
  else if (runsOpen) startRunsTimers();
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
    if (modelMenuOpen) toggleModelMenu(false);
    if (permMenuOpen) togglePermMenu(false);
    if (cmdMenuOpen) toggleCmdMenu(false);
    if (heroMenuOpen) toggleHeroMenu(false);
    if (heroModeOpen) toggleModeMenu(false);
    if (cmOpen) toggleCMPanel(false);
  }
});
// dsh ContextMeter: a pointer down outside the meter closes its open panel.
document.addEventListener("pointerdown", (e) => {
  if (cmOpen && contextMeter && !contextMeter.contains(e.target)) toggleCMPanel(false);
});
// Right details panel close button (dsh DetailsPanel close).
if (detailsCloseBtn) detailsCloseBtn.addEventListener("click", closeDetails);

boot();
