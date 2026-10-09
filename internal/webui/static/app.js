// Evals.si web UI: a read-only view over evalsid's REST API.
//
// Everything shown comes from /v1alpha1/... with the viewer's credential,
// so it shows exactly what that credential may read. Values are always
// rendered as text (textContent), never as HTML.
"use strict";

(() => {
  // ---- storage (may be unavailable: private windows, blocked storage) ----
  const store = {
    get(area, key) {
      try { return window[area].getItem(key); } catch { return null; }
    },
    set(area, key, value) {
      try {
        if (value === null) window[area].removeItem(key);
        else window[area].setItem(key, value);
      } catch { /* the session still works without it */ }
    },
  };
  let token = store.get("sessionStorage", "evalsi.token");
  let project = store.get("localStorage", "evalsi.project") || "";
  let authEnabled = true;

  // ---- DOM helpers ----
  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") node.className = v;
      else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
      else node.setAttribute(k, v === true ? "" : String(v));
    }
    for (const c of children.flat()) {
      if (c === undefined || c === null || c === false) continue;
      node.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return node;
  }
  const main = () => document.getElementById("main");
  function show(...nodes) { main().replaceChildren(...nodes.flat().filter((n) => n !== null && n !== undefined && n !== false)); }
  function table(headers, rows, numeric = []) {
    if (!rows.length) return el("p", { class: "empty" }, "Nothing here yet.");
    return el("div", { class: "scroll" }, el("table", {},
      el("thead", {}, el("tr", {}, headers.map((h, i) => el("th", { class: numeric.includes(i) ? "num" : null }, h)))),
      el("tbody", {}, rows.map((r) => el("tr", {}, r.map((c, i) => el("td", { class: numeric.includes(i) ? "num" : null }, c))))),
    ));
  }
  function kv(pairs) {
    return el("dl", { class: "kv" }, pairs.filter(([, v]) => v !== undefined && v !== "" && v !== null)
      .flatMap(([k, v]) => [el("dt", {}, k), el("dd", {}, v)]));
  }
  const fmt = (v, digits = 3) => (v === undefined || v === null || v === "" ? "–" : Number(v).toFixed(digits));
  // Proto3 JSON leaves out zeros, so an interval's missing bound is 0.
  const interval = (ci) => (ci ? `[${fmt(ci.low ?? 0)}, ${fmt(ci.high ?? 0)}]` : "–");
  const when = (ts) => (ts ? new Date(ts).toLocaleString() : "");
  const short = (s, n = 140) => (s && s.length > n ? s.slice(0, n) + "…" : s || "");
  const enumName = (v, prefix) => String(v || "").replace(prefix, "").toLowerCase().replaceAll("_", " ");
  function chips(map) {
    return Object.entries(map || {}).map(([k, v]) => el("span", { class: "chip" }, `${k}=${v}`));
  }
  function contentText(c) {
    if (!c) return "";
    if (c.text !== undefined) return c.text;
    if (c.messages) return (c.messages.messages || []).map((m) => `[${m.role}] ${m.content || ""}`).join("\n");
    const v = c.json ?? c.row ?? c.tensor ?? c.media;
    return v === undefined ? "" : JSON.stringify(v, null, 2);
  }
  // A small interval bar for metrics in [0, 1].
  function ciBar(s) {
    if (!s || s.mean === undefined || !s.ci) return "";
    const lo = Number(s.ci.low ?? 0), hi = Number(s.ci.high ?? 0), m = Number(s.mean);
    if ([lo, hi, m].some((x) => x < 0 || x > 1 || Number.isNaN(x))) return "";
    const ns = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(ns, "svg");
    svg.setAttribute("class", "ci");
    svg.setAttribute("width", "120");
    svg.setAttribute("height", "10");
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", `${fmt(lo)} to ${fmt(hi)}`);
    const rect = (cls, x, w) => {
      const r = document.createElementNS(ns, "rect");
      r.setAttribute("class", cls);
      r.setAttribute("x", String(x));
      r.setAttribute("y", cls === "mean" ? "0" : "3");
      r.setAttribute("width", String(Math.max(w, cls === "mean" ? 2 : 1)));
      r.setAttribute("height", cls === "mean" ? "10" : "4");
      return r;
    };
    svg.append(rect("track", 0, 120), rect("range", lo * 120, (hi - lo) * 120), rect("mean", m * 120 - 1, 2));
    return svg;
  }

  // ---- API ----
  class APIError extends Error {
    constructor(status, message) { super(message); this.status = status; }
  }
  async function api(path, params = {}, init = {}) {
    const url = new URL(path, window.location.origin);
    for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== "") url.searchParams.set(k, v);
    const headers = { Accept: "application/json", ...(init.headers || {}) };
    if (token) headers.Authorization = `Bearer ${token}`;
    const resp = await fetch(url, { ...init, headers, credentials: "omit" });
    const text = await resp.text();
    let body = {};
    try { body = text ? JSON.parse(text) : {}; } catch { body = { message: text }; }
    if (!resp.ok) throw new APIError(resp.status, body.message || `${resp.status} ${resp.statusText}`);
    return body;
  }
  const post = (path, body) => api(path, {}, { method: "POST", body: JSON.stringify(body), headers: { "Content-Type": "application/json" } });

  function failed(err) {
    if (err instanceof APIError && err.status === 401) return signIn("Your credential was not accepted. Sign in again.");
    if (err instanceof APIError && err.status === 403) {
      return show(el("h1", {}, "Not allowed"), el("p", { class: "error" }, err.message),
        el("p", { class: "sub" }, "Your roles do not include reading this in project " + (project || "default") + "."));
    }
    show(el("h1", {}, "Something went wrong"), el("p", { class: "error" }, String(err.message || err)));
  }

  // ---- sign-in ----
  function signIn(reason) {
    const input = el("input", { type: "password", autocomplete: "off", placeholder: "evk_... or a JWT", "aria-label": "API key or token" });
    show(el("div", { class: "card signin" },
      el("h1", {}, "Sign in"),
      reason ? el("p", { class: "error" }, reason) : null,
      el("p", {}, "Paste an API key, or the token from ", el("code", {}, "evalsi auth token --server " + window.location.origin),
        ". It is kept for this browser tab only."),
      el("form", {
        class: "inline",
        onsubmit: (e) => {
          e.preventDefault();
          token = input.value.trim();
          store.set("sessionStorage", "evalsi.token", token || null);
          start();
        },
      }, input, el("button", { class: "primary", type: "submit" }, "Sign in")),
    ));
    input.focus();
  }

  // ---- views ----
  async function viewRuns(_, query) {
    const resp = await api("/v1alpha1/runs", { project, page_size: 50, page_token: query.get("page") || "" });
    const picked = new Set();
    const compare = el("button", {
      type: "button", disabled: true,
      onclick: () => { const [a, b] = [...picked]; location.hash = `#/compare?baseline=${a}&candidate=${b}`; },
    }, "Compare selected");
    const rows = (resp.runs || []).map((r) => [
      el("input", {
        type: "checkbox", "aria-label": "select " + r.id,
        onchange: (e) => {
          if (e.target.checked) picked.add(r.id); else picked.delete(r.id);
          compare.disabled = picked.size !== 2;
        },
      }),
      el("a", { href: `#/runs/${encodeURIComponent(r.id)}` }, r.name || r.id),
      el("span", { class: `status-${r.status}` }, enumName(r.status, "RUN_STATUS_")),
      (r.gates || []).length ? el("span", { class: r.gates.every((g) => g.passed) ? "pass" : "fail" },
        `${r.gates.filter((g) => g.passed).length}/${r.gates.length}`) : "–",
      r.records || "0",
      when(r.createdAt),
      r.createdBy || "",
      chips(r.labels),
    ]);
    const next = resp.nextPageToken
      ? el("p", {}, el("a", { href: `#/runs?page=${encodeURIComponent(resp.nextPageToken)}` }, "Older runs →")) : null;
    show(el("h1", {}, "Runs"), el("p", {}, compare),
      table(["", "Run", "Status", "Gates", "Records", "Created", "By", "Labels"], rows, [4]), next);
  }

  async function viewRun(id) {
    const { run } = await api(`/v1alpha1/runs/${encodeURIComponent(id)}`);
    const spec = run.spec || {};
    const metrics = (run.summaries || []).map((s) => [
      el("code", {}, s.metric), enumName(s.kind, "METRIC_KIND_"), s.n || "0", fmt(s.mean),
      interval(s.ci), ciBar(s), s.skipped || "0", s.errors || "0",
      chips(s.labels),
    ]);
    const gates = (run.gates || []).map((g) => [
      el("code", {}, g.gate?.metric || ""), enumName(g.gate?.stat, "GATE_STAT_") || "mean",
      [g.gate?.min !== undefined ? `≥ ${g.gate.min}` : "", g.gate?.max !== undefined ? `≤ ${g.gate.max}` : ""].join(" "),
      fmt(g.value), el("span", { class: g.passed ? "pass" : "fail" }, g.passed ? "pass" : "FAIL"), g.reason || "",
    ]);
    const usage = (u) => (u ? `${u.inputTokens || 0} in, ${u.outputTokens || 0} out${u.costUsd ? `, $${Number(u.costUsd).toFixed(4)}` : ""}` : "");
    const results = el("div", {}, el("p", { class: "sub" }, "Loading results…"));
    show(
      el("h1", {}, run.name || run.id, " ", el("span", { class: `status-${run.status}` }, `(${enumName(run.status, "RUN_STATUS_")})`)),
      el("div", { class: "card" }, kv([
        ["ID", el("code", {}, run.id)], ["Project", run.project], ["Created", when(run.createdAt)], ["By", run.createdBy],
        ["Finished", when(run.finishedAt)], ["Target", spec.target ? `${spec.target.connector || ""} ${spec.target.model || ""}` : ""],
        ["Records", run.records], ["Progress", run.progress ? `${run.progress.done || 0}/${run.progress.total || 0}` : ""],
        ["Target usage", usage(run.targetUsage)], ["Judge usage", usage(run.judgeUsage)],
        ["Dataset sha256", run.datasetSha256 ? el("code", {}, run.datasetSha256) : ""], ["Labels", chips(run.labels)],
        ["Error", run.error ? el("span", { class: "error" }, run.error) : ""],
      ])),
      el("h2", {}, "Metrics"),
      table(["Metric", "Kind", "n", "Mean", "Interval", "", "Skipped", "Errors", "Labels"], metrics, [2, 3, 6, 7]),
      gates.length ? el("h2", {}, "Gates") : null, gates.length ? table(["Metric", "Stat", "Bound", "Value", "", "Reason"], gates, [3]) : null,
      el("h2", {}, "Records"), results,
    );
    loadResults(run, results).catch((err) => results.replaceChildren(el("p", { class: "error" }, String(err.message || err))));
  }

  async function loadResults(run, box) {
    const resp = await api(`/v1alpha1/runs/${encodeURIComponent(run.id)}/results`, { page_size: 500 });
    const records = new Map((resp.records || []).map((r) => [r.id, r]));
    const byRecord = new Map();
    const metrics = new Set();
    for (const res of resp.results || []) {
      const key = `${res.recordId}#${res.trial || 0}`;
      if (!byRecord.has(key)) byRecord.set(key, { id: res.recordId, trial: res.trial || 0, scores: {}, notes: [] });
      const row = byRecord.get(key);
      for (const s of res.scores || []) {
        const name = !s.name || s.name === res.evaluator ? res.evaluator : `${res.evaluator}.${s.name}`;
        const v = s.number !== undefined ? Number(s.number) : s.passed !== undefined ? (s.passed ? 1 : 0) : s.label;
        row.scores[name] = v;
        metrics.add(name);
        if (s.explanation) row.notes.push(`${name}: ${s.explanation}`);
      }
      if (res.outcome && res.outcome !== "OUTCOME_SCORED") row.notes.push(`${res.evaluator}: ${enumName(res.outcome, "OUTCOME_")} ${res.reason || ""}`);
    }
    const names = [...metrics].sort();
    const sortBy = el("select", { "aria-label": "sort by metric" }, el("option", { value: "" }, "record order"),
      names.map((n) => el("option", { value: n }, `lowest ${n} first`)));
    const holder = el("div");
    const render = () => {
      let rows = [...byRecord.values()];
      const m = sortBy.value;
      if (m) rows = rows.filter((r) => typeof r.scores[m] === "number").sort((a, b) => a.scores[m] - b.scores[m]);
      holder.replaceChildren(table(["Record", ...names, "Details"], rows.slice(0, 200).map((r) => {
        const rec = records.get(r.id) || {};
        return [
          el("code", {}, r.trial ? `${r.id} (trial ${r.trial})` : r.id),
          ...names.map((n) => (typeof r.scores[n] === "number" ? fmt(r.scores[n], 2) : r.scores[n] ?? "–")),
          el("details", {}, el("summary", {}, short(contentText(rec.input), 80) || "show"),
            ...[["input", rec.input], ["output", rec.output], ["reference", rec.reference]]
              .filter(([, c]) => c).map(([label, c]) => [el("div", { class: "sub" }, label), el("pre", { class: "content" }, contentText(c))]),
            r.notes.length ? el("pre", { class: "content" }, r.notes.join("\n")) : null),
        ];
      }), names.map((_, i) => i + 1)));
    };
    sortBy.addEventListener("change", render);
    render();
    box.replaceChildren(
      el("p", { class: "sub" }, `${byRecord.size} record result(s)${resp.nextPageToken ? " (first page)" : ""}. Sort: `, sortBy),
      holder,
    );
  }

  async function viewCompare(_, query) {
    const a = el("input", { name: "baseline", value: query.get("baseline") || "", placeholder: "baseline run id", size: 28 });
    const b = el("input", { name: "candidate", value: query.get("candidate") || "", placeholder: "candidate run id", size: 28 });
    const out = el("div");
    const form = el("form", {
      class: "inline",
      onsubmit: (e) => { e.preventDefault(); location.hash = `#/compare?baseline=${encodeURIComponent(a.value.trim())}&candidate=${encodeURIComponent(b.value.trim())}`; },
    }, a, b, el("button", { class: "primary", type: "submit" }, "Compare"));
    show(el("h1", {}, "Compare runs"), el("p", { class: "sub" }, "Paired by record: the interval is for the difference, candidate minus baseline."), form, out);
    if (!a.value || !b.value) return;
    const resp = await post("/v1alpha1/runs:compare", { baselineRunId: a.value, candidateRunId: b.value });
    out.replaceChildren(table(["Metric", "Baseline", "Candidate", "Difference", "Interval", "Paired n", ""],
      (resp.comparisons || []).map((c) => [
        el("code", {}, c.metric), fmt(c.baselineMean), fmt(c.candidateMean), fmt(c.diff),
        interval(c.diffCi), c.pairedN || "0",
        c.significant ? el("span", { class: Number(c.diff) >= 0 ? "pass" : "fail" }, Number(c.diff) >= 0 ? "▲ significant" : "▼ significant") : "",
      ]), [1, 2, 3, 5]));
  }

  async function viewPolicies() {
    const resp = await api("/v1alpha1/policies", { project });
    show(el("h1", {}, "Online policies"), table(["Policy", "Selector", "Evaluators", "Sampling", "State"],
      (resp.policies || []).map((p) => [
        el("a", { href: `#/policies/${encodeURIComponent(p.name)}` }, p.name),
        el("code", {}, short(p.selector || "all traces", 80)),
        (p.stages || []).flatMap((s) => (s.evaluators || []).map((e) => el("span", { class: "chip" }, e.name || e.ref))),
        p.sampling?.rate !== undefined ? `${Math.round(p.sampling.rate * 100)}%` : "100%",
        p.disabled ? "disabled" : "on",
      ])));
  }

  async function viewPolicy(name) {
    const { stats } = await api(`/v1alpha1/policies/${encodeURIComponent(name)}/stats`);
    const s = stats || {};
    show(el("h1", {}, name),
      el("div", { class: "card" }, kv([
        ["Traces seen", s.tracesSeen || "0"], ["Matched", s.tracesMatched || "0"], ["Sampled", s.tracesSampled || "0"],
        ["Evaluated", s.tracesEvaluated || "0"], ["Promoted", s.tracesPromoted || "0"], ["Evaluation errors", s.evaluationErrors || "0"],
      ])),
      el("p", { class: "sub" }, "Counters since the server started; metrics over the policy's window."),
      el("h2", {}, "Metrics"), table(["Metric", "n", "Mean"], (s.metrics || []).map((m) => [el("code", {}, m.metric), m.n || "0", fmt(m.mean)]), [1, 2]),
      el("h2", {}, "Alerts"), table(["Metric", "Condition", "State", "Since"], (s.alerts || []).map((a) => [
        el("code", {}, a.alert?.metric || ""),
        [a.alert?.below !== undefined ? `below ${a.alert.below}` : "", a.alert?.above !== undefined ? `above ${a.alert.above}` : ""].join(" "),
        el("span", { class: a.firing ? "fail" : "pass" }, a.firing ? "firing" : "ok"), when(a.since),
      ])));
  }

  async function viewQueues() {
    const resp = await api("/v1alpha1/queues", { project });
    show(el("h1", {}, "Annotation queues"),
      el("p", { class: "sub" }, "Annotators work through queues with ", el("code", {}, "evalsi annotate start"), "."),
      table(["Queue", "Questions", "Answers per item", "Description"], (resp.queues || []).map((q) => [
        el("a", { href: `#/queues/${encodeURIComponent(q.name)}` }, q.name),
        (q.questions || []).map((x) => el("span", { class: "chip" }, `${x.name} (${enumName(x.kind, "QUESTION_KIND_")})`)),
        q.annotationsPerItem || 1, q.description || "",
      ]), [2]));
  }

  async function viewQueue(name) {
    const s = await api(`/v1alpha1/queues/${encodeURIComponent(name)}/stats`, { project });
    show(el("h1", {}, name),
      el("div", { class: "card" }, kv([
        ["Items", `${s.done || 0} of ${s.items || 0} done`], ["Answers", s.annotations || "0"], ["Skipped", s.skipped || "0"],
        ["Annotators", chips(s.annotators)],
      ])),
      el("h2", {}, "Questions"),
      table(["Question", "n", "Mean", "Interval", "", "Agreement (α)", "Items with 2+ answers", "Vs. metric", "Labels"],
        (s.questions || []).map((q) => {
          const ag = q.metricAgreement;
          const vs = ag ? [`n=${ag.n || 0}`, ...["accuracy", "cohenKappa", "pearson", "mae"].filter((k) => ag[k] !== undefined).map((k) => `${k}=${fmt(ag[k])}`)].join(" ") : "";
          const sum = q.summary || {};
          return [el("code", {}, q.question), sum.n || "–", fmt(sum.mean), interval(sum.ci), ciBar(sum),
            fmt(q.interAnnotatorAlpha), q.multiplyAnnotated || "0", vs, chips(q.labelCounts)];
        }), [1, 2, 5, 6]));
  }

  async function viewGuardrails() {
    const resp = await api("/v1alpha1/guardrails", { project });
    show(el("h1", {}, "Guardrails"),
      el("p", { class: "sub" }, "Checks per guardrail are counted in ", el("code", {}, "/metrics"), " (evalsi_guardrail_*)."),
      ...(resp.guardrails || []).map((g) => el("div", { class: "card" },
        el("h2", {}, g.name, " ", el("span", { class: "chip" }, enumName(g.mode, "GUARDRAIL_MODE_") || "enforce")),
        kv([
          ["Description", g.description], ["Phases", (g.phases || []).map((p) => enumName(p, "GUARDRAIL_PHASE_")).join(", ") || "request, response"],
          ["Redacts", (g.redact || []).map((r) => el("span", { class: "chip" }, r.builtin || r.pattern))],
          ["Evaluators", (g.evaluators || []).map((e) => el("span", { class: "chip" }, e.name || e.ref))],
          ["Blocks when", g.blockWhen ? el("code", {}, g.blockWhen) : (g.evaluators || []).length ? "any pass/fail score fails" : ""],
          ["On failure", enumName(g.failureMode, "GUARDRAIL_FAILURE_MODE_") || "closed"], ["Timeout", g.timeout || "2s"],
          ["Updated", `${when(g.updatedAt)} ${g.updatedBy ? "by " + g.updatedBy : ""}`],
        ]))),
      (resp.guardrails || []).length ? null : el("p", { class: "empty" }, "No guardrails in this project."));
  }

  async function viewCatalog() {
    const resp = await api("/v1alpha1/evaluators");
    const all = resp.evaluators || [];
    const filter = el("input", { type: "search", placeholder: "Filter by name, pack, tier or text", size: 40, "aria-label": "filter evaluators" });
    const holder = el("div");
    const render = () => {
      const q = filter.value.trim().toLowerCase();
      const shown = all.filter((m) => !q || [m.name, m.pack, m.tier, m.runtime, m.description].join(" ").toLowerCase().includes(q));
      holder.replaceChildren(table(["Evaluator", "Pack", "Tier", "Runtime", "Outputs", "Needs", "Description"],
        shown.map((m) => [
          el("code", {}, `${m.name}@${m.version}`), m.pack || "", m.tier || "", m.runtime || "",
          (m.outputs || []).map((o) => el("span", { class: "chip" }, `${o.name}: ${enumName(o.type, "SCORE_TYPE_")}`)),
          Object.entries(m.requires || {}).filter(([k, v]) => v === true && k !== "output").map(([k]) => el("span", { class: "chip" }, k)),
          m.description || "",
        ])));
    };
    filter.addEventListener("input", render);
    render();
    show(el("h1", {}, "Evaluator catalog"), el("p", {}, filter, " ", el("span", { class: "sub" }, `${all.length} evaluators`)), holder,
      await credentialsSection());
  }

  // The worker variables (names and hosts, never values) and judges the
  // selected project's requests may use.
  async function credentialsSection() {
    const head = el("h2", {}, "Credentials in " + (project || "default"));
    let resp;
    try {
      resp = await api("/v1alpha1/credentials", { project });
    } catch (err) {
      return el("section", {}, head, el("p", { class: "sub" }, "Not shown: " + (err.message || String(err))));
    }
    const grants = resp.grants || [];
    return el("section", {}, head,
      resp.enforced ? null : el("p", { class: "sub" }, "Grants are not enforced on this server: requests may name any worker variable."),
      grants.length
        ? table(["Variable", "May be sent to", "Notes"], grants.map((g) => [
          el("code", {}, g.env),
          (g.hosts || []).length ? (g.hosts || []).map((h) => el("span", { class: "chip" }, h)) : "any host",
          [g.allowHttp ? "plain HTTP allowed" : "", g.allProjects ? "every project" : ""].filter(Boolean).join(", "),
        ]))
        : el("p", { class: "empty" }, "No worker variables granted to this project."),
      el("p", {}, "Judges: ", ...((resp.judges || []).length ? resp.judges.map((j) => el("span", { class: "chip" }, j)) : ["none"])));
  }

  // ---- routing ----
  const routes = [
    [/^\/runs\/(.+)$/, viewRun, "runs"],
    [/^\/runs$/, viewRuns, "runs"],
    [/^\/compare$/, viewCompare, "compare"],
    [/^\/policies\/(.+)$/, viewPolicy, "policies"],
    [/^\/policies$/, viewPolicies, "policies"],
    [/^\/queues\/(.+)$/, viewQueue, "queues"],
    [/^\/queues$/, viewQueues, "queues"],
    [/^\/guardrails$/, viewGuardrails, "guardrails"],
    [/^\/catalog$/, viewCatalog, "catalog"],
  ];
  async function route() {
    const raw = location.hash.replace(/^#/, "") || "/runs";
    const [path, qs] = raw.split("?");
    const query = new URLSearchParams(qs || "");
    for (const [re, view, section] of routes) {
      const m = path.match(re);
      if (!m) continue;
      for (const a of document.querySelectorAll("#nav a")) a.classList.toggle("active", a.dataset.section === section);
      show(el("p", { class: "sub" }, "Loading…"));
      try { await view(m[1] ? decodeURIComponent(m[1]) : undefined, query); } catch (err) { failed(err); }
      return;
    }
    show(el("h1", {}, "Not found"), el("p", {}, el("a", { href: "#/runs" }, "Back to runs")));
  }

  async function start() {
    try {
      const doc = await api("/.well-known/evalsi-auth");
      authEnabled = doc.auth_enabled !== false;
    } catch { authEnabled = true; }
    const signout = document.getElementById("signout");
    signout.hidden = !authEnabled || !token;
    signout.onclick = () => { token = null; store.set("sessionStorage", "evalsi.token", null); start(); };
    if (authEnabled && !token) return signIn();
    try {
      const who = await api("/v1alpha1/whoami");
      const p = who.principal || {};
      document.getElementById("whoami").textContent = p.email || p.id || p.subject || (authEnabled ? "" : "auth off");
    } catch (err) {
      if (err instanceof APIError && err.status === 401) return signIn("Your credential was not accepted.");
    }
    const select = document.getElementById("project");
    let names = ["default"];
    try {
      const resp = await api("/v1alpha1/projects");
      const listed = (resp.projects || []).map((p) => p.name);
      if (listed.length) names = listed;
    } catch { /* keep the default project */ }
    if (!names.includes(project)) project = names.includes("default") ? "default" : names[0];
    select.replaceChildren(...names.map((n) => el("option", { value: n, selected: n === project }, n)));
    select.onchange = () => { project = select.value; store.set("localStorage", "evalsi.project", project); route(); };
    window.onhashchange = route;
    route();
  }

  document.addEventListener("DOMContentLoaded", start);
})();
