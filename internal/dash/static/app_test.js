"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

class MockClassList {
  constructor() { this.values = new Set(); }
  add(...names) { names.forEach((name) => this.values.add(name)); }
  remove(...names) { names.forEach((name) => this.values.delete(name)); }
}

class MockElement {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase();
    this.children = [];
    this.className = "";
    this.classList = new MockClassList();
    this._textContent = "";
    this.hidden = false;
    this.attributes = {};
    this.parentElement = null;
    this.style = {};
    this.title = "";
    this.dateTime = "";
    this.onclick = null;
  }
  get textContent() { return this._textContent; }
  set textContent(value) {
    this._textContent = String(value);
    this.children = [];
  }
  append(...children) {
    children.forEach((child) => {
      child.parentElement = this;
      this.children.push(child);
    });
  }
  replaceChildren(...children) {
    this.children = [];
    this._textContent = "";
    this.append(...children);
  }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  click() { if (this.onclick) this.onclick(); }
}

class MockStorage {
  constructor() { this.values = new Map(); }
  getItem(key) { return this.values.has(key) ? this.values.get(key) : null; }
  setItem(key, value) { this.values.set(key, String(value)); }
}

function makeDocument() {
  const elements = new Map();
  const document = {
    title: "",
    createElement: (tag) => new MockElement(tag),
    getElementById: (id) => elements.get(id) || null,
  };
  const html = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
  for (const match of html.matchAll(/id="([^"]+)"/g)) {
    elements.set(match[1], new MockElement("span"));
  }
  document.body = new MockElement("body");
  document.body.dataset = { basePath: "/" };
  const observerParent = new MockElement("div");
  observerParent.append(elements.get("observer-state"));
  const dandelionIDs = [
    "dandelion-probe", "dandelion-attempt", "dandelion-usage-only",
    "dandelion-route", "dandelion-route-high", "dandelion-generated",
    "dandelion-notice", "dandelion-summary", "dandelion-providers",
  ];
  elements.get("dandelion-panel").append(...dandelionIDs.map((id) => elements.get(id)));
  return document;
}

global.document = makeDocument();
const app = require("./app.js");

function textOf(element) {
  if (!element) return "";
  return element.textContent + element.children.map(textOf).join(" ");
}

function findAll(element, predicate, matches = []) {
  if (predicate(element)) matches.push(element);
  element.children.forEach((child) => findAll(child, predicate, matches));
  return matches;
}

const NOW = Date.parse("2026-10-24T12:00:00Z");

function sampleSnapshot() {
  return {
    revision: 7,
    as_of: "2026-10-24T12:00:00Z",
    observer: { status: "ok", stream: "following" },
    run: {
      id: "run-1", mode: "brief", phase: "running", status: "crashed",
      ammo: 4, safety: true, elapsed_ms: 5000,
      lock: { state: "stale", running: false },
    },
    counts: {
      events: 1, beads: 0, beads_by_state: {}, agents: 0, live_agents: 0,
      reviews: 0, deliveries: 0, pull_requests: 0, merges: 0, notices: 0, activity: 0,
    },
    omitted: { beads: 0, agents: 0, reviews: 0, deliveries: 0, notices: 0, activity: 0 },
    beads: [], live_agents: [], recent_agents: [], reviews: [], deliveries: [], notices: [], activity: [],
  };
}

function sampleHost() {
  return {
    name: "build-box",
    collected_at: "2026-10-24T11:59:58Z",
    cpu: {
      used_pct: 42.4,
      cores: [{ id: 0, used_pct: 25 }, { id: 1, used_pct: 59.9 }],
      cores_omitted: 2,
    },
    memory: { used: 3 * 1024 ** 3, total: 8 * 1024 ** 3 },
    swap: { used: 512 * 1024 ** 2, total: 2 * 1024 ** 3 },
    disk: { used: 100 * 1024 ** 3, total: 1024 ** 4 },
  };
}

function sampleDandelion() {
  return {
    probing: false,
    last_attempt_at: "2026-10-24T11:59:50Z",
    last_success_at: "2026-10-24T11:59:00Z",
    stale: false,
    generated_at: "2026-10-24T11:59:00Z",
    route: "alpha-standard",
    route_high: "beta-high",
    providers_omitted: 2,
    providers: [
      {
        id: "alpha-id", display_name: "Alpha account", status: "ok", plan_label: "Pro",
        eligible: true, reason: "", fetched_at: "2026-10-24T11:58:00Z", windows_omitted: 3,
        windows: [
          {
            label: "Five hour", kind: "session", used_pct: 49.9,
            resets_at: "2026-10-24T13:00:00Z", projected_full_at: "2026-10-24T12:30:00Z",
          },
          {
            label: "Weekly", kind: "weekly", used_pct: 0,
            resets_at: "2026-10-24T11:00:00Z", projected_full_at: null,
          },
        ],
      },
      {
        id: "beta-id", display_name: "Beta account", status: "limited", plan_label: "Free",
        eligible: false, reason: "cooling down", fetched_at: "2026-10-24T11:57:00Z", windows: [],
      },
    ],
  };
}

function reset(storage = new MockStorage()) {
  global.document = makeDocument();
  global.window = { localStorage: storage };
  app._resetForTests();
  return storage;
}

test("run status remains textual and accessible", () => {
  reset();
  const snapshot = sampleSnapshot();
  app.renderRun(snapshot);
  const status = global.document.getElementById("run-status");
  assert.equal(status.textContent, "status: crashed");
  assert.equal(status.hidden, false);
  assert.equal(global.document.getElementById("run-phase").textContent, "running");
  const html = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
  assert.match(html, /id="run-status"[^>]*role="status"[^>]*aria-live="polite"[^>]*aria-atomic="true"/);
  assert.match(html, /id="run-dot"[^>]*aria-hidden="true"/);
});

test("a live agent failure is labeled as a previous attempt", () => {
  reset();
  const agent = {
    id: "eng-1", role: "engineer", state: "running", task: "implement",
    failure: "crash", elapsed_ms: 1000, live: true,
  };
  assert.match(textOf(app.agentCard(agent, true)), /previous attempt failed: crash/);
  const recent = { ...agent, state: "failed", live: false };
  const text = textOf(app.agentCard(recent, false));
  assert.match(text, /failed: crash/);
  assert.doesNotMatch(text, /previous attempt/);
});

function dandelionAgent(overrides = {}) {
  return {
    id: "eng-1", role: "engineer", state: "running", live: true,
    cartridge: "dandelion", model: "requested-configuration-model", attempt: 2,
    ...overrides,
  };
}

function subtitleFor(agent) {
  const card = app.agentCard(agent, agent.live);
  return findAll(card, (element) => element.className === "card-subtitle")[0];
}

test("Dandelion agent subtitles render routed, pending, no-route, and unknown states", () => {
  reset();
  const routed = subtitleFor(dandelionAgent({
    route_status: "routed", route_attempt: 2, route_ordinal: 1,
    routed_model: "claude-opus-5-5", routed_account: "claude", routed_effort: "max",
  }));
  assert.equal(routed.textContent, "engineer · claude-opus-5-5 · dandelion/claude · effort max");
  assert.match(routed.title, /Latest selected route for attempt 2/);
  assert.doesNotMatch(routed.textContent, /requested-configuration-model/);

  for (const [status, fallback] of [
    ["pending", "routing…"], ["no_route", "no route"], ["unknown", "route unknown"],
  ]) {
    const subtitle = subtitleFor(dandelionAgent({ route_status: status, route_attempt: 2 }));
    assert.equal(subtitle.textContent, `engineer · dandelion · ${fallback}`);
    assert.equal(subtitle.title, "");
    assert.doesNotMatch(subtitle.textContent, /requested-configuration-model/);
  }
});

test("Dandelion subtitles replace old models and never inherit routes into a new attempt", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.counts.agents = 1;
  snapshot.counts.live_agents = 1;
  snapshot.live_agents = [dandelionAgent({
    attempt: 1, route_attempt: 1, route_status: "routed", route_ordinal: 1,
    model: "requested-attempt-one", routed_model: "actual-attempt-one",
  })];
  const list = global.document.getElementById("agents-list");
  app.renderAgents(snapshot);
  assert.match(textOf(list), /actual-attempt-one/);
  assert.doesNotMatch(textOf(list), /requested-attempt-one/);

  snapshot.live_agents = [dandelionAgent({
    attempt: 2, route_attempt: 2, route_status: "routed", route_ordinal: 1,
    model: "requested-attempt-two", routed_model: "actual-attempt-two",
  })];
  app.renderAgents(snapshot);
  assert.match(textOf(list), /actual-attempt-two/);
  assert.doesNotMatch(textOf(list), /actual-attempt-one|requested-attempt-two/);

  snapshot.live_agents = [dandelionAgent({
    attempt: 3, route_attempt: 3, route_status: "pending",
    model: "requested-attempt-three", routed_model: "actual-attempt-two",
    routed_account: "stale-account", routed_effort: "stale-effort", route_ordinal: 4,
  })];
  app.renderAgents(snapshot);
  assert.match(textOf(list), /dandelion · routing…/);
  assert.doesNotMatch(textOf(list), /actual-attempt|requested-attempt|stale-account|stale-effort|latest route/);
});

test("Dandelion subtitles reject missing, stale, non-numeric, and zero attempt matches", () => {
  reset();
  for (const [status, routeAttempt, attempt] of [
    ["routed", 1, 2], ["pending", 1, 2], ["no_route", 1, 2],
    ["routed", undefined, 2], ["routed", "2", 2], ["routed", 0, 0],
  ]) {
    const subtitle = subtitleFor(dandelionAgent({
      attempt, route_attempt: routeAttempt, route_status: status,
      routed_model: "STALE-ACTUAL-MODEL", routed_account: "STALE-ACCOUNT", routed_effort: "STALE-EFFORT",
    }));
    assert.equal(subtitle.textContent, "engineer · dandelion · route unknown", `${status}, ${routeAttempt}, ${attempt}`);
    assert.equal(subtitle.title, "");
    assert.doesNotMatch(textOf(subtitle), /STALE-|requested-configuration-model/);
  }
});

test("Dandelion routed subtitles omit absent account and effort fields independently", () => {
  reset();
  const routed = {
    route_status: "routed", route_attempt: 2, routed_model: "actual-model",
  };
  assert.equal(
    subtitleFor(dandelionAgent({ ...routed, routed_effort: "high" })).textContent,
    "engineer · actual-model · dandelion · effort high",
  );
  assert.equal(
    subtitleFor(dandelionAgent({ ...routed, routed_account: "claude" })).textContent,
    "engineer · actual-model · dandelion/claude",
  );
  assert.equal(
    subtitleFor(dandelionAgent(routed)).textContent,
    "engineer · actual-model · dandelion",
  );
  assert.equal(
    subtitleFor(dandelionAgent({ ...routed, routed_account: " ", routed_effort: "" })).textContent,
    "engineer · actual-model · dandelion",
  );
});

test("Dandelion missing routed fields or status stay unknown without requested-model fallback", () => {
  reset();
  for (const agent of [
    dandelionAgent({ route_status: "routed", route_attempt: 2, routed_account: "claude", routed_effort: "max" }),
    dandelionAgent({ route_attempt: 2, routed_model: "UNTRUSTED-WITHOUT-STATUS" }),
    dandelionAgent({ route_status: "unexpected", route_attempt: 2, routed_model: "UNTRUSTED-STATUS" }),
  ]) {
    const subtitle = subtitleFor(agent);
    assert.equal(subtitle.textContent, "engineer · dandelion · route unknown");
    assert.equal(subtitle.title, "");
    assert.doesNotMatch(textOf(subtitle), /requested-configuration-model|UNTRUSTED|dandelion\/claude|effort max/);
  }
});

test("Dandelion multi-session subtitles identify the latest selected route and ordinal", () => {
  reset();
  const subtitle = subtitleFor(dandelionAgent({
    route_status: "routed", route_attempt: 2, route_ordinal: 3,
    routed_model: "latest-selected-model", routed_account: "claude", routed_effort: "max",
    routed_at: "2026-10-24T11:59:00Z", model: "older-requested-model",
  }));
  assert.equal(
    subtitle.textContent,
    "engineer · latest-selected-model · dandelion/claude · effort max · latest route 3",
  );
  assert.match(subtitle.title, /Latest selected route for attempt 2/);
  assert.match(subtitle.title, /session 3/);
  assert.match(subtitle.title, /selected /);
  assert.match(subtitle.title, /selection does not prove process start or success/);
  assert.doesNotMatch(subtitle.textContent, /older-requested-model/);
});

test("Dandelion route states ignore failure prose and global quota routes", () => {
  reset();
  const pending = subtitleFor(dandelionAgent({
    route_status: "pending", route_attempt: 2, failure: "rate_limited",
  }));
  assert.equal(pending.textContent, "engineer · dandelion · routing…");
  const unknown = subtitleFor(dandelionAgent({
    route_status: "unknown", route_attempt: 2, failure: "rate_limited",
  }));
  assert.equal(unknown.textContent, "engineer · dandelion · route unknown");

  const snapshot = sampleSnapshot();
  snapshot.counts.agents = 1;
  snapshot.counts.live_agents = 1;
  snapshot.dandelion = { route: "GLOBAL-QUOTA-ROUTE", route_high: "GLOBAL-HIGH-ROUTE" };
  snapshot.live_agents = [dandelionAgent({ route_status: "pending", route_attempt: 2 })];
  app.renderAgents(snapshot);
  const text = textOf(global.document.getElementById("agents-list"));
  assert.match(text, /dandelion · routing…/);
  assert.doesNotMatch(text, /GLOBAL-QUOTA-ROUTE|GLOBAL-HIGH-ROUTE|requested-configuration-model/);
});

test("non-Dandelion agent subtitles retain requested cartridge and model rendering", () => {
  reset();
  const subtitle = subtitleFor(dandelionAgent({
    cartridge: "claude", model: "requested-sonnet", lens: "qa",
    route_status: "routed", route_attempt: 2, route_ordinal: 2,
    routed_model: "ROUTE-FIELD-CANARY", routed_account: "ROUTE-ACCOUNT-CANARY",
  }));
  assert.equal(subtitle.textContent, "engineer/qa · claude/requested-sonnet");
  assert.equal(subtitle.title, "");
  assert.doesNotMatch(subtitle.textContent, /ROUTE-FIELD-CANARY|ROUTE-ACCOUNT-CANARY|latest route/);
});

test("Dandelion routed labels treat external strings as text", () => {
  reset();
  const markup = "<img src=x onerror=globalThis.routeXSS=true>";
  const script = "<script>globalThis.routeXSS=true</script>";
  const subtitle = subtitleFor(dandelionAgent({
    route_status: "routed", route_attempt: 2, route_ordinal: 2,
    routed_model: markup, routed_account: script, routed_effort: markup,
    routed_at: script,
  }));
  assert.match(subtitle.textContent, /engineer · <img src=x/);
  assert.match(subtitle.textContent, /dandelion\/<script>/);
  assert.match(subtitle.textContent, /effort <img src=x/);
  assert.equal(subtitle.children.length, 0);
  assert.equal(findAll(subtitle, (element) => ["IMG", "SCRIPT"].includes(element.tagName)).length, 0);
  assert.equal(findAll(subtitle, (element) => Object.keys(element.attributes).some((key) => /^on/i.test(key))).length, 0);
  assert.equal(globalThis.routeXSS, undefined);
});

test("live-agent omissions are visible with full live totals", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.counts.agents = 10;
  snapshot.counts.live_agents = 8;
  snapshot.live_agents = [
    { id: "eng-1", role: "engineer", state: "running", live: true },
    { id: "eng-2", role: "engineer", state: "running", live: true },
  ];
  snapshot.live_agents_omitted = 6;
  snapshot.omitted.agents = 8;
  app.renderAgents(snapshot);
  const text = global.document.getElementById("agent-window").textContent;
  assert.match(text, /10 seen/);
  assert.match(text, /8 live/);
  assert.match(text, /2 shown/);
  assert.match(text, /6 live omitted/);
  assert.match(text, /2 recent omitted/);
});

test("nested lens and verdict omissions are visible", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.counts.reviews = 1;
  snapshot.reviews = [{
    bead: "sc-1", number: 1, kind: "review", lenses: ["qa"],
    verdicts: [{ lens: "qa", outcome: "changes", item_count: 2 }],
    lenses_omitted: 2, verdicts_omitted: 3,
  }];
  app.renderReviews(snapshot);
  const text = textOf(global.document.getElementById("reviews-list"));
  assert.match(text, /2 lenses omitted/);
  assert.match(text, /3 verdicts omitted/);
});

test("notices render categories and never raw prose fields", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.counts.notices = 1;
  snapshot.notices = [{
    at: "2026-10-24T12:00:00Z", kind: "warning", source: "cartridge", category: "rate_limited",
    message: "NOTICE-MESSAGE-CANARY", error: "NOTICE-ERROR-CANARY",
  }];
  app.renderNotices(snapshot);
  const text = textOf(global.document.getElementById("notices-list"));
  assert.match(text, /rate limited/);
  assert.doesNotMatch(text, /NOTICE-MESSAGE-CANARY|NOTICE-ERROR-CANARY/);
});

async function runStalledPoll(withCache) {
  reset();
  if (withCache) app.render(sampleSnapshot());
  const retries = [];
  let aborted = false;
  global.window = {
    setTimeout(fn, ms) {
      if (ms === app.FETCH_TIMEOUT_MS) {
        fn();
        return 1;
      }
      retries.push({ fn, ms });
      return 2;
    },
    clearTimeout() {},
  };
  global.fetch = (_url, options) => new Promise(() => {
    options.signal.addEventListener("abort", () => { aborted = true; }, { once: true });
  });
  await app.poll();
  assert.equal(aborted, true, "the stalled fetch must be aborted");
  assert.equal(retries.length, 1, "a retry must be scheduled even though fetch never settles");
  assert.equal(retries[0].ms, 2000);
  return global.document.getElementById("observer-message").textContent;
}

test("a stalled first request aborts and reports no cache before retrying", async () => {
  const message = await runStalledPoll(false);
  assert.match(message, /timed out/);
  assert.match(message, /no cached data is available yet/);
  assert.match(message, /Retrying in 2 seconds/);
  assert.doesNotMatch(message, /showing cached data/);
});

test("a stalled refresh aborts and accurately describes cached data", async () => {
  const message = await runStalledPoll(true);
  assert.match(message, /timed out/);
  assert.match(message, /showing cached data from/);
  assert.match(message, /revision 7/);
  assert.match(message, /not a live update/);
  assert.match(message, /Retrying in 2 seconds/);
  assert.doesNotMatch(message, /no cached data/);
});

test("Dandelion balance renders a real dollar amount without a synthetic gauge", () => {
  reset();
  const card = app.dandelionProviderCard({
    id: "kilo", display_name: "Kilo", status: "ok", eligible: true,
    balance: {
      amount: 31.23, currency: "USD", used_pct: 99,
      resets_at: "2026-10-25T00:00:00Z", binding_score: 100,
    },
    windows: [],
  }, NOW);
  const text = textOf(card);
  assert.match(text, /balance \$31\.23/);
  assert.match(text, /No usage windows reported/);
  assert.equal(findAll(card, (element) => element.attributes.role === "meter").length, 0);
  assert.doesNotMatch(text, /%|used_pct|binding_score|reset in/);
  assert.equal(findAll(card, (element) => element.className === "quota-provider-balance").length, 1);
  assert.equal(app.formatDandelionBalance({ amount: 12.5, currency: "EUR" }), "balance 12.50 EUR");

  const missing = app.dandelionProviderCard({ id: "plain", display_name: "Plain", windows: [] }, NOW);
  assert.equal(findAll(missing, (element) => element.className === "quota-provider-balance").length, 0);
  assert.doesNotMatch(textOf(missing), /balance \$/);
});

test("Dandelion invalid and hostile balances are omitted", () => {
  reset();
  for (const balance of [
    undefined, null, 42, {}, { amount: 31.23 }, { currency: "USD" },
    { amount: "31.23", currency: "USD" }, { amount: null, currency: "USD" },
    { amount: -1, currency: "USD" }, { amount: Infinity, currency: "USD" },
    { amount: NaN, currency: "USD" }, { amount: 31.23, currency: null },
    { amount: 31.23, currency: 42 }, { amount: 31.23, currency: " " },
    { amount: 31.23, currency: "ABCDEFGHIJKLMNOPQ" },
    { amount: 31.23, currency: "<img src=x onerror=globalThis.balanceXSS=true>" },
    { amount: 31.23, currency: "USD\nCANARY" },
  ]) {
    assert.equal(app.normalizedDandelionBalance(balance), null);
    assert.equal(app.formatDandelionBalance(balance), "");
    const card = app.dandelionProviderCard({ id: "kilo", display_name: "Kilo", balance, windows: [] }, NOW);
    assert.equal(findAll(card, (element) => element.className === "quota-provider-balance").length, 0);
    assert.doesNotMatch(textOf(card), /CANARY|onerror/);
    assert.equal(findAll(card, (element) => ["IMG", "SCRIPT"].includes(element.tagName)).length, 0);
  }
  assert.equal(globalThis.balanceXSS, undefined);
});

test("Dandelion usage-only includes balance-bearing providers with zero windows", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.dandelion = sampleDandelion();
  snapshot.dandelion.providers = [
    { id: "kilo", display_name: "Kilo balance", status: "ok", eligible: true,
      balance: { amount: 31.232105, currency: "$" }, windows: [] },
    { id: "plain", display_name: "No quota fields", status: "ok", eligible: true, windows: [] },
    { id: "zero", display_name: "Zero usage", status: "ok", eligible: true,
      windows: [{ label: "Zero window", kind: "weekly", used_pct: 0 }] },
  ];
  app.render(snapshot, NOW);
  const providers = global.document.getElementById("dandelion-providers");
  const summary = global.document.getElementById("dandelion-summary");
  assert.match(textOf(providers), /Kilo balance/);
  assert.match(textOf(providers), /balance \$31\.23/);
  assert.doesNotMatch(textOf(providers), /\$\$31\.23/);
  const kiloCard = findAll(providers, (element) => element.className === "quota-provider")[0];
  assert.equal(findAll(kiloCard, (element) => element.attributes.role === "meter").length, 0);
  assert.doesNotMatch(textOf(kiloCard), /%|used_pct/);
  assert.match(textOf(providers), /Zero usage/);
  assert.doesNotMatch(textOf(providers), /No quota fields/);
  assert.match(summary.textContent, /2 of 3 providers shown/);
  assert.match(summary.textContent, /1 hidden by usage only/);

  app.setDandelionUsageOnly(false);
  assert.match(textOf(providers), /No quota fields/);
  assert.match(summary.textContent, /3 of 3 providers shown/);
});

test("Dandelion usage-only keeps failed probes visible without quota data", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.dandelion = sampleDandelion();
  snapshot.dandelion.providers = [
    { id: "codex", display_name: "Codex", status: "error", eligible: true,
      reason: "codex app-server exited without answering", windows: [] },
    { id: "hermes", display_name: "Hermes", status: "unavailable", eligible: true,
      reason: "token expired", windows: [] },
    { id: "plain", display_name: "No quota fields", status: "ok", eligible: true, windows: [] },
  ];
  app.render(snapshot, NOW);
  const providers = global.document.getElementById("dandelion-providers");
  assert.match(textOf(providers), /Codex/);
  assert.match(textOf(providers), /codex app-server exited without answering/);
  assert.match(textOf(providers), /Hermes/);
  assert.match(textOf(providers), /token expired/);
  assert.doesNotMatch(textOf(providers), /No quota fields/);
  assert.match(global.document.getElementById("dandelion-summary").textContent, /2 of 3 providers shown/);
});

test("Dandelion renders routes, providers, accessible meters, countdowns, and omissions", () => {
  reset();
  app.setDandelionUsageOnly(false);
  const snapshot = sampleSnapshot();
  snapshot.dandelion = sampleDandelion();
  app.render(snapshot, NOW);

  const panel = global.document.getElementById("dandelion-panel");
  assert.equal(panel.hidden, false);
  assert.equal(global.document.getElementById("dandelion-route").textContent, "alpha-standard");
  assert.equal(global.document.getElementById("dandelion-route-high").textContent, "beta-high");
  assert.equal(global.document.getElementById("dandelion-generated").textContent, "1m 0s ago");
  assert.equal(global.document.getElementById("dandelion-probe").textContent, "probe idle · last success 1m 0s ago");
  assert.equal(global.document.getElementById("dandelion-attempt").textContent, "last attempt 10s ago");
  assert.equal(global.document.getElementById("dandelion-notice").hidden, true);

  const text = textOf(panel);
  for (const expected of [
    "Alpha account", "alpha-id", "Pro", "eligible", "Beta account", "limited", "Free",
    "routing off", "cooling down", "Five hour", "session", "49.9%", "Weekly", "0%",
    "reset in 1h 0m", "projected full in 30m 0s", "reset due",
    "fetched 2m 0s ago", "3 windows omitted", "2 providers omitted", "2 of 2 providers shown",
  ]) assert.match(text, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  assert.doesNotMatch(text, /No usage windows reported.*Beta account/);
  assert.doesNotMatch(text, /reset unavailable|projected full unavailable/);

  const meters = findAll(panel, (element) => element.attributes.role === "meter");
  assert.equal(meters.length, 2);
  assert.equal(meters[0].attributes["aria-label"], "Five hour usage");
  assert.equal(meters[0].attributes["aria-valuemin"], "0");
  assert.equal(meters[0].attributes["aria-valuemax"], "100");
  assert.equal(meters[0].attributes["aria-valuenow"], "49.9");
  assert.equal(meters[0].attributes["aria-valuetext"], "49.9% used");
  const fills = findAll(panel, (element) => element.className === "quota-meter-fill");
  assert.equal(fills[0].style.width, "49.9%");
  assert.equal(fills[1].style.width, "0%");
});

test("Dandelion pending state distinguishes probing from last success", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.dandelion = {
    probing: true,
    last_attempt_at: "2026-10-24T11:59:50Z",
    providers: [],
  };
  app.renderDandelion(snapshot, NOW);

  const panel = global.document.getElementById("dandelion-panel");
  assert.equal(panel.hidden, false);
  assert.equal(global.document.getElementById("dandelion-probe").textContent, "probing · no successful probe yet");
  assert.equal(global.document.getElementById("dandelion-attempt").textContent, "last attempt 10s ago");
  assert.equal(global.document.getElementById("dandelion-generated").textContent, "—");
  assert.equal(global.document.getElementById("dandelion-route").textContent, "—");
  assert.equal(global.document.getElementById("dandelion-route-high").textContent, "—");
  assert.match(textOf(global.document.getElementById("dandelion-providers")), /First probe running; waiting for quota data/);
  assert.equal(global.document.getElementById("dandelion-notice").hidden, true);
});

test("Dandelion stale data stays visible with probing and safe timeout context", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.dandelion = {
    ...sampleDandelion(), probing: true, stale: true, error: "timed_out",
    stderr: "RAW-STDERR-CANARY", notes: "RAW-NOTES-CANARY",
  };
  app.renderDandelion(snapshot, NOW);

  const panelText = textOf(global.document.getElementById("dandelion-panel"));
  assert.match(global.document.getElementById("dandelion-probe").textContent, /^probing/);
  assert.match(panelText, /Stale quota data; showing the last successful snapshot/);
  assert.match(panelText, /latest Dandelion probe timed out/);
  assert.match(panelText, /Alpha account/);
  assert.match(panelText, /49\.9%/);
  assert.doesNotMatch(panelText, /RAW-STDERR-CANARY|RAW-NOTES-CANARY/);
  assert.match(global.document.getElementById("dandelion-notice").className, /is-error/);
});

test("Dandelion unavailable state maps only fixed safe probe errors", () => {
  reset();
  const snapshot = sampleSnapshot();
  const expectations = {
    not_installed: /not installed/,
    timed_out: /timed out/,
    probe_failed: /probe failed/,
    invalid_snapshot: /invalid quota snapshot/,
  };
  for (const [code, expected] of Object.entries(expectations)) {
    snapshot.dandelion = { probing: false, error: code, providers: [], stderr: "RAW-ERROR-CANARY" };
    app.renderDandelion(snapshot, NOW);
    const text = textOf(global.document.getElementById("dandelion-panel"));
    assert.match(text, expected);
    assert.match(text, /No last-good quota data is available; run observation is unaffected/);
    assert.doesNotMatch(text, /RAW-ERROR-CANARY/);
  }
  snapshot.dandelion = { error: "UNTRUSTED-ERROR-CANARY", providers: [] };
  app.renderDandelion(snapshot, NOW);
  const text = textOf(global.document.getElementById("dandelion-panel"));
  assert.match(text, /latest Dandelion probe could not be used/);
  assert.doesNotMatch(text, /UNTRUSTED-ERROR-CANARY/);
});

test("Dandelion disabling hides and clears all last-good panel content", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.dandelion = sampleDandelion();
  app.render(snapshot, NOW);
  const panel = global.document.getElementById("dandelion-panel");
  assert.equal(panel.hidden, false);
  assert.ok(global.document.getElementById("dandelion-providers").children.length > 0);

  delete snapshot.dandelion;
  snapshot.revision += 1;
  app.render(snapshot, NOW);
  assert.equal(panel.hidden, true);
  assert.equal(global.document.getElementById("dandelion-providers").children.length, 0);
  assert.equal(global.document.getElementById("dandelion-route").textContent, "—");
  assert.equal(global.document.getElementById("dandelion-route-high").textContent, "—");
  assert.equal(global.document.getElementById("dandelion-generated").textContent, "—");
  assert.equal(global.document.getElementById("dandelion-notice").textContent, "");
  assert.equal(global.document.getElementById("dandelion-notice").hidden, true);
  assert.equal(global.document.getElementById("dandelion-summary").textContent, "");
});

test("Dandelion usage-only defaults on, keeps zero-percent usage, and toggles from cache", () => {
  const storage = reset();
  const snapshot = sampleSnapshot();
  snapshot.dandelion = sampleDandelion();
  snapshot.dandelion.providers_omitted = 1;
  snapshot.dandelion.providers = [
    {
      id: "zero", display_name: "Zero usage", status: "ok", eligible: true,
      windows: [{ label: "Zero window", kind: "weekly", used_pct: 0 }],
    },
    { id: "empty", display_name: "No windows", status: "ok", eligible: true, windows: [] },
  ];
  app.render(snapshot, NOW);

  const button = global.document.getElementById("dandelion-usage-only");
  const providers = global.document.getElementById("dandelion-providers");
  const summary = global.document.getElementById("dandelion-summary");
  assert.equal(button.attributes["aria-pressed"], "true");
  assert.equal(storage.getItem(app.DANDELION_USAGE_KEY), null);
  assert.match(textOf(providers), /Zero usage/);
  assert.match(textOf(providers), /Zero window/);
  assert.match(textOf(providers), /0%/);
  assert.doesNotMatch(textOf(providers), /No windows/);
  assert.match(summary.textContent, /1 of 2 providers shown/);
  assert.match(summary.textContent, /1 hidden by usage only/);
  assert.match(summary.textContent, /1 provider omitted/);

  let fetchCalls = 0;
  global.fetch = () => { fetchCalls += 1; };
  button.click();
  assert.equal(button.attributes["aria-pressed"], "false");
  assert.equal(storage.getItem(app.DANDELION_USAGE_KEY), "0");
  assert.equal(fetchCalls, 0, "the filter must reuse the cached snapshot");
  assert.match(textOf(providers), /Zero usage/);
  assert.match(textOf(providers), /No windows/);
  assert.match(summary.textContent, /2 of 2 providers shown/);
  assert.doesNotMatch(summary.textContent, /hidden by usage only/);

  button.click();
  assert.equal(button.attributes["aria-pressed"], "true");
  assert.equal(storage.getItem(app.DANDELION_USAGE_KEY), "1");
  assert.equal(fetchCalls, 0);
  assert.doesNotMatch(textOf(providers), /No windows/);
});

test("Dandelion usage-only preference persists across dashboard initialization", () => {
  const storage = reset();
  app.setDandelionUsageOnly(false);
  assert.equal(storage.getItem(app.DANDELION_USAGE_KEY), "0");

  reset(storage);
  assert.equal(global.document.getElementById("dandelion-usage-only").attributes["aria-pressed"], "false");
  const snapshot = sampleSnapshot();
  snapshot.dandelion = sampleDandelion();
  app.renderDandelion(snapshot, NOW);
  assert.match(textOf(global.document.getElementById("dandelion-providers")), /Beta account/);
});

test("Dandelion render handles absent and partial quota data", () => {
  reset();
  const snapshot = sampleSnapshot();
  const panel = global.document.getElementById("dandelion-panel");
  for (const value of [undefined, null, false, [], "invalid"]) {
    snapshot.dandelion = value;
    assert.doesNotThrow(() => app.render(snapshot, NOW));
    assert.equal(panel.hidden, true);
  }

  snapshot.dandelion = {
    providers_omitted: 1,
    providers: [null, {
      id: "partial-id",
      windows: [null, { label: "Partial window", used_pct: null }],
    }],
  };
  assert.doesNotThrow(() => app.render(snapshot, NOW));
  assert.equal(panel.hidden, false);
  const text = textOf(panel);
  assert.match(text, /partial-id/);
  assert.match(text, /unknown/);
  assert.match(text, /eligible/);
  assert.doesNotMatch(text, /routing off/);
  assert.match(text, /Partial window/);
  assert.match(text, /0%/);
  assert.match(text, /1 provider omitted/);
  assert.equal(global.document.getElementById("dandelion-route").textContent, "—");
  assert.equal(global.document.getElementById("dandelion-generated").textContent, "—");
});

test("Dandelion stale state without last-good data stays explicit and safe", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.dandelion = {
    probing: false, stale: true, error: "invalid_snapshot", providers: [],
    response: "RAW-RESPONSE-CANARY",
  };
  app.render(snapshot, NOW);
  const notice = global.document.getElementById("dandelion-notice");
  assert.match(notice.textContent, /Quota data is stale; no last-good snapshot is available/);
  assert.match(notice.textContent, /invalid quota snapshot/);
  assert.doesNotMatch(notice.textContent, /RAW-RESPONSE-CANARY/);
  assert.match(textOf(global.document.getElementById("dandelion-providers")), /No last-good quota data is available/);
});

test("Dandelion shows singular provider and window omissions without usage", () => {
  reset();
  app.setDandelionUsageOnly(false);
  const snapshot = sampleSnapshot();
  snapshot.dandelion = {
    last_success_at: "2026-10-24T11:59:00Z",
    providers_omitted: 1,
    providers: [{ id: "bounded", windows: [], windows_omitted: 1 }],
  };
  app.renderDandelion(snapshot, NOW);
  const text = textOf(global.document.getElementById("dandelion-panel"));
  assert.match(text, /No usage windows reported/);
  assert.match(text, /1 window omitted/);
  assert.doesNotMatch(text, /1 windows omitted/);
  assert.match(text, /1 provider omitted/);
  assert.doesNotMatch(text, /1 providers omitted/);
});

test("Dandelion tones and accessible values follow clamped threshold boundaries", () => {
  reset();
  const cases = [
    [-25, 0, "calm", "0%"], [0, 0, "calm", "0%"],
    [49.9, 49.9, "calm", "49.9%"], [50, 50, "warm", "50%"],
    [79.9, 79.9, "warm", "79.9%"], [80, 80, "hot", "80%"],
    [94.9, 94.9, "hot", "94.9%"], [95, 95, "crit", "95%"],
    [100, 100, "crit", "100%"], [125, 100, "crit", "100%"],
  ];
  for (const [input, clamped, tone, formatted] of cases) {
    assert.equal(app.clampDandelionPercentage(input), clamped);
    assert.equal(app.formatDandelionPercentage(input), formatted);
    assert.equal(app.dandelionTone(input), tone);
    const card = app.dandelionWindowCard({ label: "Boundary", used_pct: input }, NOW);
    assert.equal(card.className, `quota-window quota-tone-${tone}`);
    const meter = findAll(card, (element) => element.attributes.role === "meter")[0];
    assert.equal(meter.attributes["aria-label"], "Boundary usage");
    assert.equal(meter.attributes["aria-valuenow"], String(clamped));
    assert.equal(meter.attributes["aria-valuetext"], `${formatted} used`);
    const fill = findAll(card, (element) => element.className === "quota-meter-fill")[0];
    assert.equal(fill.style.width, `${clamped}%`);
  }
  for (const value of [undefined, null, NaN, Infinity, -Infinity, "50", {}, []]) {
    assert.equal(app.clampDandelionPercentage(value), 0);
    assert.equal(app.dandelionTone(value), "calm");
  }
});

test("Dandelion high-precision thresholds share normalized display, tone, width, and ARIA values", () => {
  reset();
  const cases = [
    [49.94, 49.9, "calm", "49.9%"], [49.96, 50, "warm", "50%"],
    [50.04, 50, "warm", "50%"], [50.06, 50.1, "warm", "50.1%"],
    [79.94, 79.9, "warm", "79.9%"], [79.96, 80, "hot", "80%"],
    [80.04, 80, "hot", "80%"], [80.06, 80.1, "hot", "80.1%"],
    [94.94, 94.9, "hot", "94.9%"], [94.96, 95, "crit", "95%"],
    [95.04, 95, "crit", "95%"], [95.06, 95.1, "crit", "95.1%"],
  ];
  for (const [input, normalized, tone, formatted] of cases) {
    assert.equal(app.formatDandelionPercentage(input), formatted, `format ${input}`);
    assert.equal(app.dandelionTone(input), tone, `tone ${input}`);
    const card = app.dandelionWindowCard({ label: "Precision", used_pct: input }, NOW);
    assert.equal(card.className, `quota-window quota-tone-${tone}`, `card tone ${input}`);
    const percent = findAll(card, (element) => element.className === "quota-percent")[0];
    assert.equal(percent.textContent, formatted, `visible percentage ${input}`);
    const fill = findAll(card, (element) => element.className === "quota-meter-fill")[0];
    assert.equal(fill.style.width, `${normalized}%`, `meter width ${input}`);
    const meter = findAll(card, (element) => element.attributes.role === "meter")[0];
    assert.equal(meter.attributes["aria-valuenow"], String(normalized), `aria-valuenow ${input}`);
    assert.equal(meter.attributes["aria-valuetext"], `${formatted} used`, `aria-valuetext ${input}`);
  }
});

test("Dandelion countdowns handle future, past, missing, and invalid dates", () => {
  reset();
  const future = "2026-10-24T12:00:30Z";
  const past = "2026-10-24T11:59:30Z";
  assert.equal(app.dandelionAge(future, NOW), "0s ago");
  assert.equal(app.dandelionAge("not-a-date", NOW), "");
  assert.equal(app.dandelionAge("0001-01-01T00:00:00Z", NOW), "");
  assert.equal(app.dandelionCountdown(future, NOW), "reset in 30s");
  assert.equal(app.dandelionCountdown(future, NOW, true), "projected full in 30s");
  assert.equal(app.dandelionCountdown(past, NOW), "reset due");
  assert.equal(app.dandelionCountdown(past, NOW, true), "projected full now");
  assert.equal(app.dandelionCountdown(null, NOW), "");
  assert.equal(app.dandelionCountdown(undefined, NOW), "");
  assert.equal(app.dandelionCountdown("not-a-date", NOW, true), "");

  const validCard = app.dandelionWindowCard({
    label: "Dates", used_pct: 0, resets_at: future, projected_full_at: past,
  }, NOW);
  const validTimes = findAll(validCard, (element) => element.tagName === "TIME");
  assert.equal(validTimes[0].dateTime, "2026-10-24T12:00:30.000Z");
  assert.equal(validTimes[1].dateTime, "2026-10-24T11:59:30.000Z");
  for (const element of validTimes) {
    assert.notEqual(element.title, "");
    assert.notEqual(element.title, "unknown");
  }

  const invalidCard = app.dandelionWindowCard({
    label: "Dates", used_pct: 0, resets_at: "bad", projected_full_at: "0001-01-01T00:00:00Z",
  }, NOW);
  for (const element of findAll(invalidCard, (child) => child.tagName === "TIME")) {
    assert.equal(element.textContent, "");
    assert.equal(element.dateTime, "");
    assert.equal(element.title, "");
  }
});

test("Dandelion treats external strings as text and ignores raw or unknown fields", () => {
  reset();
  app.setDandelionUsageOnly(false);
  const markup = "<img src=x onerror=globalThis.quotaXSS=true>";
  const script = "<script>globalThis.quotaXSS=true</script>";
  const raw = "RAW-QUOTA-CANARY";
  const snapshot = sampleSnapshot();
  snapshot.dandelion = sampleDandelion();
  Object.assign(snapshot.dandelion, {
    route: markup, route_high: script, error: raw,
    response: raw, notes: raw, balances: raw, routesError: raw, stderr: raw,
  });
  const provider = snapshot.dandelion.providers[0];
  Object.assign(provider, {
    id: script, display_name: markup, status: markup, plan_label: script,
    reason: markup, notes: raw, balance: raw, error: raw,
  });
  Object.assign(provider.windows[0], { label: markup, kind: script, raw: raw });
  app.render(snapshot, NOW);

  const panel = global.document.getElementById("dandelion-panel");
  const route = global.document.getElementById("dandelion-route");
  assert.equal(route.textContent, markup);
  assert.equal(route.children.length, 0);
  const name = findAll(panel, (element) => element.className === "quota-provider-name")[0];
  assert.equal(name.textContent, markup);
  assert.equal(name.children.length, 0);
  assert.equal(findAll(panel, (element) => ["IMG", "SCRIPT"].includes(element.tagName)).length, 0);
  assert.equal(findAll(panel, (element) => Object.keys(element.attributes).some((key) => /^on/i.test(key))).length, 0);
  const text = textOf(panel);
  assert.match(text, /latest Dandelion probe could not be used/);
  assert.doesNotMatch(text, /RAW-QUOTA-CANARY/);
  assert.equal(globalThis.quotaXSS, undefined);

  const source = fs.readFileSync(path.join(__dirname, "app.js"), "utf8");
  assert.doesNotMatch(source, /\b(?:innerHTML|outerHTML|insertAdjacentHTML)\b|\beval\s*\(|\bdocument\.write\s*\(/);
});

function relativeLuminance(hex) {
  const channels = [1, 3, 5].map((offset) => parseInt(hex.slice(offset, offset + 2), 16) / 255).map((value) => {
    return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
  });
  return channels[0] * 0.2126 + channels[1] * 0.7152 + channels[2] * 0.0722;
}

function contrastRatio(foreground, background) {
  const a = relativeLuminance(foreground);
  const b = relativeLuminance(background);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

test("Dandelion meaningful text and toggle colors meet contrast requirements", () => {
  const css = fs.readFileSync(path.join(__dirname, "style.css"), "utf8");
  const colors = {};
  for (const match of css.matchAll(/--([\w-]+):\s*(#[0-9a-f]{6})/g)) colors[match[1]] = match[2];
  colors.hot = css.match(/\.quota-tone-hot\s*\{\s*color:\s*(#[0-9a-f]{6})/)[1];
  for (const foreground of ["text", "muted", "faint", "amber", "green", "cyan", "red", "hot"]) {
    for (const background of ["bg", "surface", "surface-raised"]) {
      const ratio = contrastRatio(colors[foreground], colors[background]);
      assert.ok(ratio >= 4.5, `${foreground} on ${background} has ${ratio.toFixed(2)}:1 contrast`);
    }
  }
  assert.ok(contrastRatio(colors.bg, colors.green) >= 4.5, "active usage toggle text must remain readable");
});

test("Dandelion mobile rules preserve a single-column usable layout", () => {
  const css = fs.readFileSync(path.join(__dirname, "style.css"), "utf8");
  const mobileAt = css.indexOf("@media (max-width: 620px)");
  assert.notEqual(mobileAt, -1);
  const mobile = css.slice(mobileAt);
  for (const rule of [
    ".dandelion-head { flex-wrap: wrap; }",
    ".dandelion-route-grid, .dandelion-providers { grid-template-columns: minmax(0, 1fr); }",
    ".quota-provider-head { display: grid; }",
    ".quota-window-times { display: grid; }",
  ]) assert.ok(mobile.includes(rule), `missing mobile rule: ${rule}`);
  assert.match(css, /\.usage-toggle:focus-visible\s*\{[^}]*outline:/);
  const html = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
  assert.match(html, /name="viewport" content="width=device-width, initial-scale=1"/);
  assert.match(html, /id="dandelion-usage-only"[^>]*aria-pressed="true"[^>]*aria-controls="dandelion-providers"/);
});

test("host byte formatting uses binary units and rejects invalid values", () => {
  const cases = [
    [0, "0 B"], [1023, "1023 B"], [1024, "1 KiB"], [1536, "1.5 KiB"],
    [1024 ** 2, "1 MiB"], [8 * 1024 ** 3, "8 GiB"], [1024 ** 4, "1 TiB"],
  ];
  for (const [value, expected] of cases) assert.equal(app.formatBinaryBytes(value), expected);
  for (const value of [undefined, null, -1, NaN, Infinity, "1024", {}, []]) {
    assert.equal(app.formatBinaryBytes(value), "—");
  }
});

test("host metrics render binary totals, accessible CPU, cores, and collection time", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.host = sampleHost();
  app.renderHost(snapshot);

  assert.equal(global.document.getElementById("host-content").hidden, false);
  assert.equal(global.document.getElementById("host-unsupported").hidden, true);
  assert.equal(global.document.getElementById("host-name").textContent, "build-box");
  assert.equal(global.document.getElementById("host-state").textContent, "live · 2s samples");
  assert.equal(global.document.getElementById("host-cpu-value").textContent, "42.4%");
  assert.equal(global.document.getElementById("host-memory-value").textContent, "3 GiB / 8 GiB");
  assert.equal(global.document.getElementById("host-swap-value").textContent, "512 MiB / 2 GiB");
  assert.equal(global.document.getElementById("host-swap-resource").hidden, false);
  assert.equal(global.document.getElementById("host-disk-value").textContent, "100 GiB / 1 TiB");

  const gauge = global.document.getElementById("host-cpu-gauge");
  assert.equal(gauge.hidden, false);
  const html = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
  assert.match(html, /id="host-cpu-gauge"[^>]*role="meter"[^>]*aria-label="Aggregate CPU usage"[^>]*aria-valuemin="0"[^>]*aria-valuemax="100"/);
  assert.equal(gauge.attributes["aria-valuenow"], "42.4");
  assert.equal(gauge.attributes["aria-valuetext"], "42.4% used");
  assert.equal(global.document.getElementById("host-cpu-fill").style.width, "42.4%");

  const cores = global.document.getElementById("host-cores");
  assert.equal(cores.children.length, 2);
  assert.match(textOf(cores), /core 0 25%/);
  assert.match(textOf(cores), /core 1 59\.9%/);
  assert.equal(global.document.getElementById("host-core-note").textContent, "2 cores · 2 omitted");
  const collected = global.document.getElementById("host-collected");
  assert.equal(collected.dateTime, "2026-10-24T11:59:58.000Z");
  assert.notEqual(collected.title, "");
  assert.match(collected.textContent, /^sampled /);
});

test("host sampling, zero-swap omission, and unsupported states clear stale values", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.host = sampleHost();
  delete snapshot.host.cpu;
  snapshot.host.swap.total = 0;
  app.renderHost(snapshot);

  assert.equal(global.document.getElementById("host-state").textContent, "sampling");
  assert.equal(global.document.getElementById("host-cpu-value").textContent, "sampling");
  assert.equal(global.document.getElementById("host-cpu-gauge").hidden, true);
  assert.equal(global.document.getElementById("host-cores").children.length, 0);
  assert.equal(global.document.getElementById("host-swap-resource").hidden, true);
  assert.doesNotMatch(textOf(global.document.getElementById("host-content")), /512 MiB/);

  snapshot.host = null;
  app.renderHost(snapshot);
  assert.equal(global.document.getElementById("host-content").hidden, true);
  assert.equal(global.document.getElementById("host-unsupported").hidden, false);
  assert.equal(global.document.getElementById("host-state").textContent, "unsupported");
  assert.equal(global.document.getElementById("host-memory-value").textContent, "—");
  assert.equal(global.document.getElementById("host-disk-value").textContent, "—");
  assert.equal(global.document.getElementById("host-collected").textContent, "");
});

test("host cache is labeled last-good on refresh errors and live after recovery", () => {
  reset();
  const snapshot = sampleSnapshot();
  snapshot.host = sampleHost();
  app.render(snapshot, NOW);
  app.refreshError(false);
  assert.equal(global.document.getElementById("host-state").textContent, "last-good");
  assert.equal(global.document.getElementById("host-memory-value").textContent, "3 GiB / 8 GiB");
  assert.equal(global.document.getElementById("host-cpu-value").textContent, "42.4%");

  snapshot.revision += 1;
  app.render(snapshot, NOW);
  assert.equal(global.document.getElementById("host-state").textContent, "live · 2s samples");
  assert.equal(global.document.getElementById("observer-message").hidden, true);
});

test("host fields remain text and the responsive panel precedes run metrics", () => {
  reset();
  const markup = "<img src=x onerror=globalThis.hostXSS=true>";
  const snapshot = sampleSnapshot();
  snapshot.host = sampleHost();
  snapshot.host.name = markup;
  snapshot.host.cpu.cores[0].id = "<script>globalThis.hostXSS=true</script>";
  app.renderHost(snapshot);
  const panel = global.document.getElementById("host-panel");
  assert.equal(global.document.getElementById("host-name").textContent, markup);
  assert.match(textOf(global.document.getElementById("host-cores")), /<script>/);
  assert.equal(findAll(panel, (element) => ["IMG", "SCRIPT"].includes(element.tagName)).length, 0);
  assert.equal(globalThis.hostXSS, undefined);

  const html = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
  assert.ok(html.indexOf('id="host-panel"') < html.indexOf('aria-label="Run totals"'));
  const css = fs.readFileSync(path.join(__dirname, "style.css"), "utf8");
  assert.match(css, /\.host-resources\s*\{[^}]*repeat\(auto-fit, minmax\(160px, 1fr\)\)/);
  assert.match(css, /\.host-cores\s*\{[^}]*grid-template-columns: repeat\(auto-fill, minmax\(120px, 1fr\)\)/);
  assert.match(css, /\.host-name\s*\{[^}]*overflow-wrap: anywhere/);
});
