(() => {
  "use strict";

  const base = document.body.dataset.basePath || "/";
  const api = (route) => (base === "/" ? "" : base) + route;
  const byID = (id) => document.getElementById(id);
  let lastRevision = 0;

  function node(tag, className, text) {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (text !== undefined && text !== null) element.textContent = String(text);
    return element;
  }

  function paintBadge(element, text, kind) {
    const safeKind = String(kind || text || "unknown").toLowerCase().replace(/[^a-z0-9_]+/g, "-");
    element.className = `badge badge-${safeKind}`;
    element.textContent = String(text || "—").replaceAll("_", " ");
  }

  function badge(text, kind) {
    const element = node("span");
    paintBadge(element, text, kind);
    return element;
  }

  function setText(id, value) {
    byID(id).textContent = String(value);
  }

  function empty(text, tag = "div") {
    return node(tag, "empty", text);
  }

  function duration(ms) {
    let seconds = Math.max(0, Math.floor(Number(ms || 0) / 1000));
    const days = Math.floor(seconds / 86400);
    seconds %= 86400;
    const hours = Math.floor(seconds / 3600);
    seconds %= 3600;
    const minutes = Math.floor(seconds / 60);
    seconds %= 60;
    if (days) return `${days}d ${hours}h`;
    if (hours) return `${hours}h ${minutes}m`;
    if (minutes) return `${minutes}m ${seconds}s`;
    return `${seconds}s`;
  }

  function clock(value) {
    if (!value) return "—";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "—";
    return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }

  function fullTime(value) {
    if (!value) return "unknown";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "unknown";
    return date.toLocaleString();
  }

  function safeLink(url, text) {
    try {
      const parsed = new URL(String(url));
      if (parsed.protocol !== "https:" && parsed.protocol !== "http:") throw new Error("unsupported");
      const link = node("a", "cell-id", text);
      link.href = parsed.href;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      return link;
    } catch {
      return node("span", "cell-id", text);
    }
  }

  function omittedNote(total, shown, omitted, noun) {
    const baseNote = `${total} ${noun}`;
    return omitted ? `${baseNote} · latest ${shown} · ${omitted} omitted` : baseNote;
  }

  function renderRun(snapshot) {
    const run = snapshot.run || {};
    const statusText = String(run.status || "unknown");
    const statusKind = statusText.replace(/[^a-z]/g, "") || "unknown";
    setText("run-id", run.id || "unknown run");
    document.title = `${run.id || "sc"} · run observer`;
    paintBadge(byID("run-status"), `status: ${statusText}`, statusKind);
    paintBadge(byID("run-phase"), run.phase || "new", statusKind);
    const dot = byID("run-dot");
    dot.className = "status-dot";
    dot.classList.add(statusKind);
    byID("run-quiet").hidden = !run.quiet;
    byID("run-quiet").textContent = run.quiet ? `quiet ${duration(Number(run.quiet_for_seconds || 0) * 1000)}` : "quiet";
    setText("run-mode", run.mode || "—");
    setText("run-ammo", `${run.ammo || 0} ammo`);
    setText("run-safety", `safety ${run.safety ? "on" : "off"}`);
    const lock = run.lock || {};
    const lockDetail = lock.pid_reused ? " · pid reused" : lock.running ? " · process live" : "";
    setText("run-lock", `lock ${lock.state || "unknown"}${lockDetail}`);
    setText("run-elapsed", `${duration(run.elapsed_ms)} elapsed`);
    setText("observer-state", `${snapshot.observer.status} · ${snapshot.observer.stream} · run ${statusText}`);
    byID("observer-state").parentElement.classList.remove("error");
    setText("snapshot-time", `snapshot ${fullTime(snapshot.as_of)} · revision ${snapshot.revision}`);
  }

  function renderMetrics(snapshot) {
    const counts = snapshot.counts || {};
    const states = counts.beads_by_state || {};
    const active = Object.entries(states).reduce((sum, [state, count]) => {
      return ["closed", "failed", "needs_human"].includes(state) ? sum : sum + Number(count || 0);
    }, 0);
    setText("metric-beads", counts.beads || 0);
    setText("metric-beads-note", `${active} in flow · ${states.closed || 0} closed`);
    setText("metric-agents", counts.live_agents || 0);
    setText("metric-agents-note", `${counts.agents || 0} seen`);
    setText("metric-reviews", counts.reviews || 0);
    setText("metric-reviews-note", counts.reviews ? "feedback loop" : "awaiting work");
    setText("metric-prs", counts.pull_requests || 0);
    setText("metric-delivery-note", `${counts.merges || 0} merged`);
    setText("metric-events", counts.events || 0);
    setText("metric-events-note", `${counts.activity || 0} lifecycle`);
  }

  function formatBinaryBytes(value) {
    if (typeof value !== "number" || !Number.isFinite(value) || value < 0) return "—";
    const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"];
    let scaled = value;
    let unit = 0;
    while (scaled >= 1024 && unit < units.length - 1) {
      scaled /= 1024;
      unit += 1;
    }
    const precision = unit > 0 && scaled < 10 ? 1 : 0;
    const formatted = scaled.toFixed(precision).replace(/\.0$/, "");
    return `${formatted} ${units[unit]}`;
  }

  function hostUsage(value) {
    const usage = value && typeof value === "object" && !Array.isArray(value) ? value : {};
    return `${formatBinaryBytes(usage.used)} / ${formatBinaryBytes(usage.total)}`;
  }

  function hostStatus(text, kind) {
    const status = byID("host-state");
    status.className = `badge host-state host-state-${kind}`;
    status.textContent = text;
  }

  function clearHost() {
    setText("host-name", "unsupported host");
    hostStatus("unsupported", "unsupported");
    byID("host-content").hidden = true;
    byID("host-unsupported").hidden = false;
    setText("host-memory-value", "—");
    setText("host-swap-value", "—");
    setText("host-disk-value", "—");
    byID("host-swap-resource").hidden = true;
    setText("host-cpu-value", "—");
    setText("host-cpu-note", "CPU sampling unavailable");
    byID("host-cpu-gauge").hidden = true;
    byID("host-cpu-fill").style.width = "0%";
    setText("host-core-note", "unavailable");
    byID("host-cores").replaceChildren();
    const collected = byID("host-collected");
    collected.textContent = "";
    collected.dateTime = "";
    collected.title = "";
  }

  function renderHost(snapshot, lastGood = false) {
    const host = snapshot && snapshot.host;
    if (!host || typeof host !== "object" || Array.isArray(host)) {
      clearHost();
      return;
    }

    byID("host-content").hidden = false;
    byID("host-unsupported").hidden = true;
    setText("host-name", quotaText(host.name, "unknown host"));
    setText("host-memory-value", hostUsage(host.memory));
    setText("host-disk-value", hostUsage(host.disk));
    const swap = host.swap && typeof host.swap === "object" && !Array.isArray(host.swap) ? host.swap : null;
    const showSwap = Boolean(swap && typeof swap.total === "number" && Number.isFinite(swap.total) && swap.total > 0);
    byID("host-swap-resource").hidden = !showSwap;
    setText("host-swap-value", showSwap ? hostUsage(swap) : "—");

    const collected = byID("host-collected");
    const collectedAt = validQuotaDate(host.collected_at);
    collected.textContent = collectedAt ? `sampled ${collectedAt.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}` : "sampled —";
    collected.dateTime = collectedAt ? collectedAt.toISOString() : "";
    collected.title = collectedAt ? collectedAt.toLocaleString() : "";

    const cpu = host.cpu && typeof host.cpu === "object" && !Array.isArray(host.cpu) ? host.cpu : null;
    const hasCPU = Boolean(cpu && typeof cpu.used_pct === "number" && Number.isFinite(cpu.used_pct));
    const gauge = byID("host-cpu-gauge");
    const cores = byID("host-cores");
    const rows = [];
    if (!hasCPU) {
      hostStatus(lastGood ? "last-good" : "sampling", lastGood ? "last-good" : "sampling");
      setText("host-cpu-value", "sampling");
      setText("host-cpu-note", "awaiting a second matching CPU sample");
      gauge.hidden = true;
      gauge.setAttribute("aria-valuetext", "CPU sampling in progress");
      byID("host-cpu-fill").style.width = "0%";
      setText("host-core-note", "awaiting second sample");
    } else {
      hostStatus(lastGood ? "last-good" : "live · 2s samples", lastGood ? "last-good" : "live");
      const percentage = normalizeDandelionPercentage(cpu.used_pct);
      const formatted = formatDandelionPercentage(percentage);
      setText("host-cpu-value", formatted);
      setText("host-cpu-note", "aggregate usage");
      gauge.hidden = false;
      gauge.setAttribute("aria-valuenow", String(percentage));
      gauge.setAttribute("aria-valuetext", `${formatted} used`);
      byID("host-cpu-fill").style.width = `${percentage}%`;

      quotaObjects(cpu.cores).forEach((core, index) => {
        const percentage = normalizeDandelionPercentage(core.used_pct);
        const rawID = typeof core.id === "string" || typeof core.id === "number" ? String(core.id).trim() : "";
        const id = rawID || String(index);
        const row = node("div", "host-core");
        row.append(node("span", "", `core ${id}`));
        row.append(node("strong", "", formatDandelionPercentage(percentage)));
        const track = node("span", "host-meter-track");
        track.setAttribute("aria-hidden", "true");
        const fill = node("span", "host-meter-fill");
        fill.style.width = `${percentage}%`;
        track.append(fill);
        row.append(track);
        rows.push(row);
      });
      const omitted = quotaCount(cpu.cores_omitted);
      const notes = [];
      if (rows.length) notes.push(`${rows.length} ${rows.length === 1 ? "core" : "cores"}`);
      if (omitted) notes.push(`${omitted} omitted`);
      setText("host-core-note", notes.join(" · ") || "per-core data unavailable");
    }
    cores.replaceChildren(...rows);
  }

  function renderBeads(snapshot) {
    const body = byID("beads-body");
    const rows = [];
    const beads = snapshot.beads || [];
    if (!beads.length) {
      const row = node("tr");
      const cell = empty("No beads in the fold yet", "td");
      cell.colSpan = 4;
      row.append(cell);
      rows.push(row);
    }
    beads.forEach((bead) => {
      const row = node("tr");
      const identity = node("td");
      identity.append(node("span", "cell-id", bead.id));
      identity.append(node("span", "cell-title", bead.title || "untitled"));
      const state = node("td");
      state.append(badge(bead.state || "unrecorded"));
      const round = node("td", "cell-number", bead.round ? `r${bead.round}` : "—");
      const delivery = node("td");
      if (bead.pr_url) delivery.append(safeLink(bead.pr_url, `PR #${bead.pr_number || "?"}`));
      else if (bead.pr_number) delivery.append(node("span", "cell-id", `PR #${bead.pr_number}`));
      else delivery.append(node("span", "cell-number", "—"));
      row.append(identity, state, round, delivery);
      rows.push(row);
    });
    body.replaceChildren(...rows);

    const counts = byID("bead-counts");
    const chips = [];
    Object.entries(snapshot.counts.beads_by_state || {}).forEach(([state, count]) => {
      const chip = node("span", "state-count", state.replaceAll("_", " "));
      chip.append(node("strong", "", count));
      chips.push(chip);
    });
    counts.replaceChildren(...chips);
    setText("bead-window", omittedNote(snapshot.counts.beads, beads.length, snapshot.omitted.beads, "beads"));
  }

  function agentSubtitle(agent) {
    const role = [agent.role, agent.lens].filter(Boolean).join("/");
    const subtitle = node("div", "card-subtitle");
    if (agent.cartridge !== "dandelion") {
      const engine = [agent.cartridge, agent.model].filter(Boolean).join("/");
      subtitle.textContent = [role, engine].filter(Boolean).join(" · ") || "agent";
      return subtitle;
    }

    const exactAttempt = Number.isInteger(agent.attempt) && agent.attempt > 0 && agent.route_attempt === agent.attempt;
    const routedModel = typeof agent.routed_model === "string" ? agent.routed_model.trim() : "";
    if (exactAttempt && agent.route_status === "routed" && routedModel) {
      const parts = [role, routedModel];
      const account = typeof agent.routed_account === "string" ? agent.routed_account.trim() : "";
      const effort = typeof agent.routed_effort === "string" ? agent.routed_effort.trim() : "";
      parts.push(account ? `dandelion/${account}` : "dandelion");
      if (effort) parts.push(`effort ${effort}`);
      const ordinal = Number.isInteger(agent.route_ordinal) && agent.route_ordinal > 0 ? agent.route_ordinal : 0;
      if (ordinal > 1) parts.push(`latest route ${ordinal}`);
      subtitle.textContent = parts.filter(Boolean).join(" · ");

      const details = [`Latest selected route for attempt ${agent.attempt}`];
      if (ordinal) details.push(`session ${ordinal}`);
      if (agent.routed_at) {
        const selectedAt = fullTime(agent.routed_at);
        if (selectedAt !== "unknown") details.push(`selected ${selectedAt}`);
      }
      details.push("selection does not prove process start or success");
      subtitle.title = details.join(" · ");
      return subtitle;
    }

    let fallback = "route unknown";
    if (exactAttempt && agent.route_status === "pending") fallback = "routing…";
    else if (exactAttempt && agent.route_status === "no_route") fallback = "no route";
    subtitle.textContent = [role, "dandelion", fallback].filter(Boolean).join(" · ");
    return subtitle;
  }

  function agentCard(agent, live) {
    const card = node("article", `card ${live ? "card-live" : "card-recent"}`);
    const top = node("div", "card-top");
    top.append(node("span", "card-title", agent.id));
    top.append(badge(agent.last_seen ? "last seen" : agent.state, agent.state));
    const subtitle = agentSubtitle(agent);
    const detail = node("div", "card-detail");
    const task = [agent.task, agent.bead].filter(Boolean).join(" · ");
    detail.textContent = task || "run-level work";
    const bottom = node("div", "card-bottom");
    const qualifiers = [];
    if (agent.round) qualifiers.push(`round ${agent.round}`);
    if (agent.attempt) qualifiers.push(`attempt ${agent.attempt}`);
    if (agent.failure) qualifiers.push(live ? `previous attempt failed: ${agent.failure}` : `failed: ${agent.failure}`);
    if (agent.produced) qualifiers.push(agent.produced);
    bottom.append(node("span", "", qualifiers.join(" · ") || clock(agent.started_at)));
    bottom.append(node("span", "", duration(agent.elapsed_ms)));
    card.append(top, subtitle, detail, bottom);
    return card;
  }

  function renderAgents(snapshot) {
    const live = snapshot.live_agents || [];
    const recent = snapshot.recent_agents || [];
    const cards = [];
    live.forEach((agent) => cards.push(agentCard(agent, true)));
    recent.forEach((agent) => cards.push(agentCard(agent, false)));
    if (!cards.length) cards.push(empty("No agents fired yet"));
    byID("agents-list").replaceChildren(...cards);
    const liveOmitted = snapshot.live_agents_omitted || 0;
    const recentOmitted = snapshot.omitted.agents - liveOmitted;
    const notes = [`${snapshot.counts.agents || 0} seen`, `${snapshot.counts.live_agents || 0} live`, `${live.length + recent.length} shown`];
    if (liveOmitted) notes.push(`${liveOmitted} live omitted`);
    if (recentOmitted) notes.push(`${recentOmitted} recent omitted`);
    setText("agent-window", notes.join(" · "));
  }

  function renderReviews(snapshot) {
    const reviews = snapshot.reviews || [];
    const cards = [];
    reviews.slice().reverse().forEach((review) => {
      const card = node("article", "card");
      const top = node("div", "card-top");
      top.append(node("span", "card-title", `${review.bead} · round ${review.number}`));
      top.append(badge(review.kind || "review"));
      const verdicts = node("div", "inline-badges");
      const verdictList = review.verdicts || [];
      verdictList.forEach((verdict) => {
        verdicts.append(badge(`${verdict.lens || "review"}: ${verdict.outcome} · ${verdict.item_count}`, verdict.outcome));
      });
      if (!verdictList.length) {
        (review.lenses || []).forEach((lens) => verdicts.append(badge(lens, "review")));
      }
      const bottom = node("div", "card-bottom");
      const counts = [`${review.response_count || 0} responses`, `${review.ruling_count || 0} rulings`];
      if (review.has_report) counts.push("report received");
      if (review.escalation) counts.push(review.escalation.replaceAll("_", " "));
      bottom.append(node("span", "", counts.join(" · ")));
      bottom.append(node("span", "", review.commit ? review.commit.slice(0, 7) : ""));
      card.append(top, verdicts);
      if (review.lenses_omitted || review.verdicts_omitted) {
        const omissions = [];
        if (review.lenses_omitted) omissions.push(`${review.lenses_omitted} lenses omitted`);
        if (review.verdicts_omitted) omissions.push(`${review.verdicts_omitted} verdicts omitted`);
        card.append(node("div", "card-detail", omissions.join(" · ")));
      }
      card.append(bottom);
      cards.push(card);
    });
    if (!cards.length) cards.push(empty("No review rounds yet"));
    byID("reviews-list").replaceChildren(...cards);
    setText("review-window", omittedNote(snapshot.counts.reviews, reviews.length, snapshot.omitted.reviews, "rounds"));
  }

  function renderDeliveries(snapshot) {
    const deliveries = snapshot.deliveries || [];
    const cards = [];
    deliveries.slice().reverse().forEach((delivery) => {
      const card = node("article", "card");
      const top = node("div", "card-top");
      if (delivery.url) top.append(safeLink(delivery.url, `${delivery.bead} · PR #${delivery.number || "?"}`));
      else top.append(node("span", "card-title", delivery.bead));
      top.append(badge(delivery.merged ? "merged" : delivery.number ? "open" : "delivery", delivery.merged ? "merged" : "delivering"));
      const subtitle = node("div", "card-subtitle", [delivery.branch, delivery.base].filter(Boolean).join(" → ") || "direct delivery");
      const bottom = node("div", "card-bottom");
      const flags = [];
      if (delivery.external) flags.push("external PR");
      if (delivery.pushed) flags.push("pushed");
      bottom.append(node("span", "", flags.join(" · ")));
      bottom.append(node("span", "", delivery.sha ? delivery.sha.slice(0, 7) : ""));
      card.append(top, subtitle, bottom);
      cards.push(card);
    });
    if (!cards.length) cards.push(empty("No pull requests or merges yet"));
    byID("delivery-list").replaceChildren(...cards);
    setText("delivery-window", omittedNote(snapshot.counts.deliveries, deliveries.length, snapshot.omitted.deliveries, "deliveries"));
  }

  function renderNotices(snapshot) {
    const notices = snapshot.notices || [];
    const rows = [];
    notices.slice().reverse().forEach((notice) => {
      const row = node("article", `event event-notice-${notice.kind}`);
      row.append(node("time", "event-time", clock(notice.at)));
      const body = node("div", "event-body");
      body.append(node("div", "event-meta", `${notice.kind} · ${notice.source || "run"}`));
      body.append(node("div", "event-text", (notice.category || "generic").replaceAll("_", " ")));
      row.append(body);
      rows.push(row);
    });
    if (!rows.length) rows.push(empty("No warnings or errors"));
    byID("notices-list").replaceChildren(...rows);
    setText("notice-window", omittedNote(snapshot.counts.notices, notices.length, snapshot.omitted.notices, "notices"));
  }

  function renderActivity(snapshot) {
    const activity = snapshot.activity || [];
    const rows = [];
    activity.slice().reverse().forEach((event) => {
      const row = node("article", "event");
      row.append(node("time", "event-time", clock(event.at)));
      const body = node("div", "event-body");
      body.append(node("div", "event-meta", event.kind.replaceAll("_", " ")));
      body.append(node("div", "event-text", event.text));
      row.append(body);
      rows.push(row);
    });
    if (!rows.length) rows.push(empty("Waiting for lifecycle events"));
    byID("activity-list").replaceChildren(...rows);
    setText("activity-window", omittedNote(snapshot.counts.activity, activity.length, snapshot.omitted.activity, "events") + " · latest first");
  }

  const DANDELION_USAGE_KEY = "dash-dandelion-usage-only";
  const DANDELION_ERROR_MESSAGES = Object.freeze({
    not_installed: "Dandelion is not installed; quota probing is unavailable.",
    timed_out: "The latest Dandelion probe timed out.",
    probe_failed: "The latest Dandelion probe failed.",
    invalid_snapshot: "Dandelion returned an invalid quota snapshot.",
  });

  function dandelionStorage() {
    try {
      return typeof window === "undefined" ? null : window.localStorage || null;
    } catch {
      return null;
    }
  }

  function readDandelionUsageOnly() {
    try {
      const storage = dandelionStorage();
      return !storage || storage.getItem(DANDELION_USAGE_KEY) !== "0";
    } catch {
      return true;
    }
  }

  let dandelionUsageOnly = readDandelionUsageOnly();

  function syncDandelionUsageToggle() {
    const button = byID("dandelion-usage-only");
    if (!button) return;
    button.setAttribute("aria-pressed", String(dandelionUsageOnly));
    button.title = dandelionUsageOnly ? "Hide providers without usage windows or balances" : "Show only providers with usage windows or balances";
    button.onclick = toggleDandelionUsageOnly;
  }

  function setDandelionUsageOnly(value) {
    dandelionUsageOnly = Boolean(value);
    try {
      const storage = dandelionStorage();
      if (storage) storage.setItem(DANDELION_USAGE_KEY, dandelionUsageOnly ? "1" : "0");
    } catch {}
    syncDandelionUsageToggle();
    if (lastSnapshot) renderDandelion(lastSnapshot);
  }

  function toggleDandelionUsageOnly() {
    setDandelionUsageOnly(!dandelionUsageOnly);
  }

  function quotaText(value, fallback = "") {
    return typeof value === "string" && value.trim() ? value : fallback;
  }

  function validQuotaDate(value) {
    if (typeof value !== "string" || !value.trim()) return null;
    const date = new Date(value);
    if (!Number.isFinite(date.getTime()) || date.getUTCFullYear() <= 1) return null;
    return date;
  }

  function quotaNow(value) {
    if (value instanceof Date && Number.isFinite(value.getTime())) return value.getTime();
    if (typeof value === "number" && Number.isFinite(value)) return value;
    const date = validQuotaDate(value);
    return date ? date.getTime() : Date.now();
  }

  function dandelionAge(value, now) {
    const date = validQuotaDate(value);
    if (!date) return "";
    return `${duration(Math.max(0, quotaNow(now) - date.getTime()))} ago`;
  }

  function dandelionCountdown(value, now, projected = false) {
    const date = validQuotaDate(value);
    const label = projected ? "projected full" : "reset";
    if (!date) return "";
    const remaining = date.getTime() - quotaNow(now);
    if (remaining <= 0) return projected ? "projected full now" : "reset due";
    return `${label} in ${duration(remaining)}`;
  }

  function clampDandelionPercentage(value) {
    const number = typeof value === "number" && Number.isFinite(value) ? value : 0;
    return Math.max(0, Math.min(100, number));
  }

  function normalizeDandelionPercentage(value) {
    return Math.round(clampDandelionPercentage(value) * 10) / 10;
  }

  function formatDandelionPercentage(value) {
    return `${normalizeDandelionPercentage(value)}%`;
  }

  function dandelionTone(value) {
    const percentage = normalizeDandelionPercentage(value);
    if (percentage >= 95) return "crit";
    if (percentage >= 80) return "hot";
    if (percentage >= 50) return "warm";
    return "calm";
  }

  function dandelionErrorMessage(code) {
    if (!code) return "";
    const message = typeof code === "string" ? DANDELION_ERROR_MESSAGES[code] : "";
    return typeof message === "string" ? message : "The latest Dandelion probe could not be used.";
  }

  function quotaCount(value) {
    return typeof value === "number" && Number.isFinite(value) && value > 0 ? Math.floor(value) : 0;
  }

  function quotaObjects(values) {
    return Array.isArray(values) ? values.filter((value) => value && typeof value === "object" && !Array.isArray(value)) : [];
  }

  function normalizedDandelionBalance(value) {
    if (!value || typeof value !== "object" || Array.isArray(value)) return null;
    if (typeof value.amount !== "number" || !Number.isFinite(value.amount) || value.amount < 0) return null;
    if (typeof value.currency !== "string") return null;
    let currency = value.currency.trim();
    if (currency === "$") currency = "USD";
    if (!currency || currency.length > 16 || !/^[A-Za-z0-9 ._-]+$/.test(currency)) return null;
    return { amount: value.amount, currency };
  }

  function formatDandelionBalance(value) {
    const balance = normalizedDandelionBalance(value);
    if (!balance) return "";
    const amount = balance.amount.toFixed(2);
    return balance.currency.toUpperCase() === "USD" ? `balance $${amount}` : `balance ${amount} ${balance.currency}`;
  }

  function setQuotaTime(element, value) {
    const date = validQuotaDate(value);
    if (date) {
      element.dateTime = date.toISOString();
      element.title = fullTime(value);
    }
    return element;
  }

  function dandelionWindowCard(window, now) {
    const label = quotaText(window.label, "usage window");
    const kind = quotaText(window.kind);
    const percentage = normalizeDandelionPercentage(window.used_pct);
    const tone = dandelionTone(percentage);
    const card = node("article", `quota-window quota-tone-${tone}`);
    const heading = node("div", "quota-window-head");
    heading.append(node("span", "quota-window-label", label));
    if (kind) heading.append(node("span", "quota-window-kind", kind.replaceAll("_", " ")));

    const meter = node("div", "quota-meter");
    meter.setAttribute("role", "meter");
    meter.setAttribute("aria-label", `${label} usage`);
    meter.setAttribute("aria-valuemin", "0");
    meter.setAttribute("aria-valuemax", "100");
    meter.setAttribute("aria-valuenow", String(percentage));
    meter.setAttribute("aria-valuetext", `${formatDandelionPercentage(percentage)} used`);
    const track = node("span", "quota-meter-track");
    track.setAttribute("aria-hidden", "true");
    const fill = node("span", "quota-meter-fill");
    fill.style.width = `${percentage}%`;
    track.append(fill);
    meter.append(track, node("strong", "quota-percent", formatDandelionPercentage(percentage)));

    const times = node("div", "quota-window-times");
    const reset = node("time", "", dandelionCountdown(window.resets_at, now));
    const projected = node("time", "", dandelionCountdown(window.projected_full_at, now, true));
    times.append(setQuotaTime(reset, window.resets_at), setQuotaTime(projected, window.projected_full_at));
    card.append(heading, meter, times);
    return card;
  }

  function dandelionProviderCard(provider, now) {
    const name = quotaText(provider.display_name, quotaText(provider.id, "unknown provider"));
    const id = quotaText(provider.id);
    const status = quotaText(provider.status, "unknown");
    const plan = quotaText(provider.plan_label);
    const eligible = provider.eligible !== false;
    const card = node("article", `quota-provider${eligible ? "" : " quota-provider-off"}`);
    const heading = node("div", "quota-provider-head");
    const identity = node("div", "quota-provider-identity");
    identity.append(node("strong", "quota-provider-name", name));
    if (id && id !== name) identity.append(node("span", "quota-provider-id", id));
    const badges = node("div", "quota-provider-badges");
    badges.append(badge(status, status));
    if (plan) badges.append(badge(plan, "plan"));
    badges.append(badge(eligible ? "eligible" : "routing off", eligible ? "eligible" : "routing-off"));
    heading.append(identity, badges);
    card.append(heading);

    const reason = quotaText(provider.reason);
    if (reason) card.append(node("p", "quota-provider-reason", reason));
    const balance = formatDandelionBalance(provider.balance);
    if (balance) card.append(node("div", "quota-provider-balance", balance));
    const windows = quotaObjects(provider.windows);
    if (windows.length) {
      const list = node("div", "quota-windows");
      windows.forEach((window) => list.append(dandelionWindowCard(window, now)));
      card.append(list);
    } else {
      card.append(node("div", "quota-no-windows", "No usage windows reported"));
    }

    const fetchedAge = dandelionAge(provider.fetched_at, now);
    const omitted = quotaCount(provider.windows_omitted);
    if (fetchedAge || omitted) {
      const footer = node("div", "quota-provider-footer");
      if (fetchedAge) footer.append(setQuotaTime(node("time", "", `fetched ${fetchedAge}`), provider.fetched_at));
      if (omitted) footer.append(node("span", "", `${omitted} ${omitted === 1 ? "window" : "windows"} omitted`));
      card.append(footer);
    }
    return card;
  }

  function clearDandelion() {
    byID("dandelion-panel").hidden = true;
    byID("dandelion-providers").replaceChildren();
    setText("dandelion-route", "—");
    setText("dandelion-route-high", "—");
    setText("dandelion-generated", "—");
    setText("dandelion-probe", "probe idle · no successful probe yet");
    setText("dandelion-attempt", "last attempt —");
    byID("dandelion-generated").title = "";
    byID("dandelion-probe").title = "";
    byID("dandelion-attempt").title = "";
    const notice = byID("dandelion-notice");
    notice.hidden = true;
    notice.textContent = "";
    notice.className = "dandelion-notice";
    const summary = byID("dandelion-summary");
    summary.hidden = true;
    summary.textContent = "";
  }

  function renderDandelion(snapshot, now) {
    syncDandelionUsageToggle();
    const quota = snapshot && snapshot.dandelion;
    if (!quota || typeof quota !== "object" || Array.isArray(quota)) {
      clearDandelion();
      return;
    }

    const currentNow = quotaNow(now);
    const providers = quotaObjects(quota.providers);
    const successAge = dandelionAge(quota.last_success_at, currentNow);
    const attemptAge = dandelionAge(quota.last_attempt_at, currentNow);
    const generatedAge = dandelionAge(quota.generated_at, currentNow);
    const hasData = Boolean(successAge || generatedAge || providers.length || quotaText(quota.route) || quotaText(quota.route_high));
    const errorMessage = dandelionErrorMessage(quota.error);
    byID("dandelion-panel").hidden = false;
    setText("dandelion-route", quotaText(quota.route, "—"));
    setText("dandelion-route-high", quotaText(quota.route_high, "—"));
    setText("dandelion-generated", generatedAge || "—");
    byID("dandelion-generated").title = validQuotaDate(quota.generated_at) ? fullTime(quota.generated_at) : "";
    setText("dandelion-probe", `${quota.probing === true ? "probing" : "probe idle"} · ${successAge ? `last success ${successAge}` : "no successful probe yet"}`);
    setText("dandelion-attempt", `last attempt ${attemptAge || "—"}`);
    byID("dandelion-probe").title = validQuotaDate(quota.last_success_at) ? `last success ${fullTime(quota.last_success_at)}` : "";
    byID("dandelion-attempt").title = validQuotaDate(quota.last_attempt_at) ? fullTime(quota.last_attempt_at) : "";

    const messages = [];
    if (quota.stale === true) messages.push(hasData ? "Stale quota data; showing the last successful snapshot." : "Quota data is stale; no last-good snapshot is available.");
    if (errorMessage) messages.push(errorMessage);
    const notice = byID("dandelion-notice");
    notice.textContent = messages.join(" ");
    notice.hidden = !messages.length;
    notice.className = `dandelion-notice${errorMessage ? " is-error" : ""}`;

    const usageProviders = providers.filter((provider) => quotaObjects(provider.windows).length > 0 || normalizedDandelionBalance(provider.balance) !== null || ["error", "unavailable"].includes(quotaText(provider.status).toLowerCase()));
    const shown = dandelionUsageOnly ? usageProviders : providers;
    const hiddenCount = providers.length - shown.length;
    const omitted = quotaCount(quota.providers_omitted);
    const summaryParts = [];
    if (providers.length) summaryParts.push(`${shown.length} of ${providers.length} ${providers.length === 1 ? "provider" : "providers"} shown`);
    if (hiddenCount) summaryParts.push(`${hiddenCount} hidden by usage only`);
    if (omitted) summaryParts.push(`${omitted} ${omitted === 1 ? "provider" : "providers"} omitted`);
    const summary = byID("dandelion-summary");
    summary.textContent = summaryParts.join(" · ");
    summary.hidden = !summaryParts.length;

    const cards = shown.map((provider) => dandelionProviderCard(provider, currentNow));
    if (!cards.length) {
      let message = "Waiting for the first successful Dandelion probe.";
      if (providers.length && dandelionUsageOnly) message = "No providers have usage windows or balances. Turn off usage only to show all providers.";
      else if (hasData) message = "No provider quota data was reported.";
      else if (quota.probing === true) message = "First probe running; waiting for quota data.";
      else if (errorMessage) message = "No last-good quota data is available; run observation is unaffected.";
      cards.push(empty(message));
    }
    byID("dandelion-providers").replaceChildren(...cards);
  }

  let lastSnapshot = null;

  function render(snapshot, now) {
    renderRun(snapshot);
    renderMetrics(snapshot);
    renderHost(snapshot);
    renderBeads(snapshot);
    renderAgents(snapshot);
    renderReviews(snapshot);
    renderDeliveries(snapshot);
    renderNotices(snapshot);
    renderActivity(snapshot);
    renderDandelion(snapshot, now);
    byID("observer-message").hidden = true;
    lastRevision = snapshot.revision;
    lastSnapshot = snapshot;
  }

  function refreshError(timedOut) {
    const reason = timedOut ? "Snapshot refresh timed out" : "Snapshot refresh failed";
    const message = byID("observer-message");
    if (lastSnapshot) {
      message.textContent = `${reason}; showing cached data from ${fullTime(lastSnapshot.as_of)} (revision ${lastRevision}), not a live update. Retrying in 2 seconds.`;
      byID("observer-state").textContent = "refresh stale · cached snapshot only";
      renderHost(lastSnapshot, true);
    } else {
      message.textContent = `${reason}; no cached data is available yet. Retrying in 2 seconds.`;
      byID("observer-state").textContent = "refresh unavailable · no cached snapshot";
    }
    message.hidden = false;
    byID("observer-state").parentElement.classList.add("error");
  }

  const POLL_INTERVAL_MS = 2000;
  const FETCH_TIMEOUT_MS = 8000;

  async function poll() {
    const controller = new AbortController();
    let timedOut = false;
    let timer;
    const request = (async () => {
      const response = await fetch(api("/api/snapshot"), {
        method: "GET",
        headers: { Accept: "application/json" },
        cache: "no-store",
        signal: controller.signal,
      });
      if (!response.ok) throw new Error(`snapshot returned ${response.status}`);
      return response.json();
    })();
    const timeout = new Promise((_, reject) => {
      timer = window.setTimeout(() => {
        timedOut = true;
        controller.abort();
        reject(new Error("snapshot refresh timed out"));
      }, FETCH_TIMEOUT_MS);
    });
    try {
      const snapshot = await Promise.race([request, timeout]);
      if (!lastRevision || snapshot.revision >= lastRevision) render(snapshot);
    } catch {
      refreshError(timedOut);
    } finally {
      window.clearTimeout(timer);
      window.setTimeout(poll, POLL_INTERVAL_MS);
    }
  }

  syncDandelionUsageToggle();

  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      DANDELION_USAGE_KEY,
      POLL_INTERVAL_MS,
      FETCH_TIMEOUT_MS,
      agentCard,
      clampDandelionPercentage,
      dandelionAge,
      dandelionCountdown,
      dandelionErrorMessage,
      dandelionProviderCard,
      dandelionTone,
      dandelionWindowCard,
      formatBinaryBytes,
      formatDandelionBalance,
      formatDandelionPercentage,
      node,
      normalizedDandelionBalance,
      poll,
      refreshError,
      render,
      renderAgents,
      renderDandelion,
      renderHost,
      renderNotices,
      renderReviews,
      renderRun,
      setDandelionUsageOnly,
      _resetForTests() {
        lastRevision = 0;
        lastSnapshot = null;
        dandelionUsageOnly = readDandelionUsageOnly();
        clearDandelion();
        syncDandelionUsageToggle();
      },
    };
  } else {
    poll();
  }
})();
