// Release configuration and read-only case investigation. Uses the same API,
// credential and text-only DOM helpers as the rest of the UI.
"use strict";
window.evalsiWorkbench = ({ el, api, post, show, field, table, kv, fmt, contentText, notice, download, scope, active }) => {
  const json = v => JSON.stringify(v, null, 2);
  const pre = v => el("pre", { class: "content", tabindex: 0 }, typeof v === "string" ? v : json(v));
  const input = (value = "", attrs = {}) => el("input", { value, ...attrs });
  const number = (value = "", attrs = {}) => input(value, { type: "number", step: "any", ...attrs });
  const select = entries => el("select", {}, entries.map(([value, label]) => el("option", { value }, label)));
  const button = (label, fn, attrs = {}) => el("button", { type: "button", onclick: fn, ...attrs }, label);
  const attemptKey = () => "browser-" + Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, "0")).join("");
  const exactKey = (id, trial) => JSON.stringify([id, Number(trial || 0)]);
  const caseLink = (baseline, candidate, record, trial, project = scope) => `#/case?${new URLSearchParams({ project, baseline, candidate, record, trial: String(trial || 0) })}`;
  const workbenchLink = params => `#/workbench?${new URLSearchParams(params)}`;
  const canonical = v => {
    if (Array.isArray(v)) return v.map(canonical);
    if (v && typeof v === "object") return Object.fromEntries(Object.keys(v).sort().map(k => [k, canonical(v[k])]));
    return v;
  };
  const behavior = step => Object.fromEntries(["type", "name", "input", "output", "error"].filter(k => step[k] !== undefined).map(k => [k, step[k]]));
  const same = (a, b) => JSON.stringify(canonical(behavior(a))) === JSON.stringify(canonical(behavior(b)));
  const displayContent = c => !c ? "Unavailable" : c.messages ? json(c.messages) : contentText(c) || "(empty recorded content)";

  function stepView(step, index) {
    return el("details", {}, el("summary", {}, `${index + 1}. ${(step.type || "generic").replace("STEP_TYPE_", "").toLowerCase()} · ${step.name || "unnamed"}${step.error ? " · ERROR" : ""}`),
      kv([["Started", step.startTime || "Unavailable"], ["Ended", step.endTime || "Unavailable"], ["Usage", step.usage ? json(step.usage) : "Unavailable"], ["Error", step.error]]),
      el("h3", {}, "Arguments / input"), pre(displayContent(step.input)), el("h3", {}, "Result / output"), pre(displayContent(step.output)),
      step.attributes ? el("details", {}, el("summary", {}, "Recorded attributes"), pre(step.attributes)) : null);
  }
  function recordPanel(record, results = [], outputError = "") {
    return el("div", {},
      ...["input", "output", "reference"].map(key => el("details", { open: key === "output" }, el("summary", {}, key), pre(displayContent(record[key])))),
      outputError ? el("p", { class: "error" }, `Execution error: ${outputError}`) : null,
      kv([["Stop reason", record.metadata?.agent?.stop_reason || "Unavailable"], ["Usage", record.usage ? json(record.usage) : "Unavailable"]]),
      el("h3", {}, "Evaluator outcomes"), table(["Evaluator", "Outcome", "Scores / explanation"], results.map(r => [r.evaluator, (r.outcome || "OUTCOME_UNSPECIFIED").replace("OUTCOME_", ""), pre({ scores: r.scores || [], reason: r.reason || "" })])),
      el("h3", {}, "Checker outcome"), record.check ? el("div", {}, el("p", { class: record.check.error ? "error" : record.check.passed ? "pass" : "fail" }, record.check.error ? "Checker error: " + record.check.error : record.check.passed ? "Checker passed" : "Checker failed"), pre({ passed: record.check.passed === true, ...record.check })) : el("p", { class: "sub" }, "Unavailable"),
      el("h3", {}, "Recorded events"), ...(record.trajectory?.steps || []).map(stepView),
      !record.trajectory?.steps?.length ? el("p", { class: "sub" }, "No recorded trajectory is available.") : null);
  }

  async function inspect(_, query) {
    const baseline = query.get("baseline") || "", candidate = query.get("candidate") || "", record = query.get("record") || "";
    const form = el("form", { class: "inline" });
    const a = input(baseline), b = input(candidate, { required: true }), r = input(record, { required: true });
    const baselineTrial = query.get("baseline_trial") || query.get("trial") || "0";
    const candidateTrial = query.get("candidate_trial") || query.get("trial") || "0";
    const at = number(baselineTrial, { min: 0, step: 1, required: true });
    const bt = number(candidateTrial, { min: 0, step: 1, required: true });
    form.append(field("Baseline run (optional)", a), field("Candidate run", b), field("Record ID", r), field("Baseline trial", at), field("Candidate trial", bt), el("button", { type: "submit" }, "Inspect case"));
    form.onsubmit = event => { event.preventDefault(); location.hash = `#/case?${new URLSearchParams({ project: scope, baseline: a.value, candidate: b.value, record: r.value, baseline_trial: at.value, candidate_trial: bt.value })}`; };
    const content = el("div");
    show(el("h1", {}, "Paired case inspector"), form, el("p", { class: "sub" }, "Reads stored evidence. Trial numbers identify executions, not matching random seeds. Missing telemetry stays unavailable."), content);
    if (!candidate || !record) return;
    // Settle every read before rendering either side: denial on either run must
    // never leave the other run's private content visible in a paired view.
    const ids = [baseline, candidate].filter(Boolean);
    const out = await Promise.allSettled(ids.map(async (id, i) => {
      const { run } = await api(`/v1alpha1/runs/${encodeURIComponent(id)}`);
      const trial = id === candidate && (!baseline || i === 1) ? candidateTrial : baselineTrial;
      try { return { run, ...(await api(`/v1alpha1/runs/${encodeURIComponent(id)}/record`, { record_id: record, trial })) }; }
      catch (err) { if (![404, 412].includes(err.status)) throw err; return { run, unavailable: err.message }; }
    }));
    if (!active()) return;
    for (const entry of out) if (entry.status === "rejected" && ![404, 412].includes(entry.reason.status)) throw entry.reason;
    const values = out.map(entry => entry.status === "fulfilled" ? entry.value : { unavailable: entry.reason.message });
    content.append(el("div", { class: "paired-evidence" }, values.map((v, i) => el("section", { class: "card", "aria-label": baseline && i === 0 ? "Baseline case" : "Candidate case" },
      el("h2", {}, baseline && i === 0 ? "Baseline" : "Candidate"), kv([["Run", v.run?.id || ids[i]], ["Project", v.run?.project || scope], ["Declared commit", v.run?.labels?.["agent.commit"] || "Unavailable"], ["Trial", baseline && i === 0 ? baselineTrial : candidateTrial], ["Evidence source", v.evidence?.outputError ? "Execution failed; showing the admitted dataset snapshot" : v.evidence?.outputStored ? "Stored execution output" : "Admitted dataset snapshot"]]),
      v.unavailable ? el("p", { class: "sub" }, `Evidence unavailable: ${v.unavailable}`) : recordPanel(v.evidence.record, v.results, v.evidence.outputError)))));
    if (values.length === 2 && values.every(v => v.evidence)) {
      const left = values[0].evidence.record.trajectory?.steps || [], right = values[1].evidence.record.trajectory?.steps || [];
      content.append(el("h2", {}, "Event sequence differences"), el("p", { class: "sub" }, "Ordinal alignment compares event type, name, input, output and error. Insertions shift positions, so alignment is approximate. Timing, usage and span IDs remain in the lanes; they do not mark a behavioral change. Differences do not establish causation."),
        table(["Position", "Baseline event", "Candidate event", "Difference"], Array.from({ length: Math.max(left.length, right.length) }, (_, i) => [i + 1, left[i]?.name || "Absent", right[i]?.name || "Absent", !left[i] ? "Added" : !right[i] ? "Removed" : same(left[i], right[i]) ? "Unchanged" : "Changed (approximate alignment)"])));
    }
  }

  function parameterControls(schema, initial = {}) {
    const readers = [], holder = el("div");
    for (const [name, def] of Object.entries(schema?.properties || {})) {
      let node, read;
      const value = initial[name];
      if (def.enum) { node = select([["", "Default"], ...def.enum.map(v => [json(v), String(v)])]); if (value !== undefined) node.value = json(value); read = () => node.value === "" ? undefined : JSON.parse(node.value); }
      else if (def.type === "boolean") { node = select([["", "Default"], ["true", "True"], ["false", "False"]]); if (value !== undefined) node.value = String(value); read = () => node.value === "" ? undefined : node.value === "true"; }
      else if (["number", "integer"].includes(def.type)) { node = number(value ?? "", { min: def.minimum, max: def.maximum, step: def.type === "integer" ? 1 : "any" }); read = () => node.value === "" ? undefined : Number(node.value); }
      else if (def.type === "string") { node = input(value ?? ""); read = () => node.value === "" && value === undefined ? undefined : node.value; }
      else { node = el("textarea", { rows: 2, placeholder: "Optional structured JSON" }, value === undefined ? "" : json(value)); read = () => node.value.trim() ? JSON.parse(node.value) : undefined; }
      holder.append(field(`Parameter ${name}`, node));
      readers.push(() => [name, read()]);
    }
    return { node: holder, read: () => Object.fromEntries(readers.map(read => read()).filter(([, v]) => v !== undefined)) };
  }

  async function view(_, query) {
    const [catalog, credentials] = await Promise.all([api("/v1alpha1/evaluators"), api("/v1alpha1/credentials", { project: scope })]);
    if (!active()) return;
    const dataset = select([["", "Select an approved suite"]]), suiteOptions = new Map();
    let datasetCursor = "", runCursor = "";
    const status = el("div"), candidateStatus = el("div"), runOptions = el("datalist", { id: "workbench-runs" });
    const baseline = input(query.get("baseline") || "", { list: "workbench-runs" }), candidate = input(query.get("candidate") || "", { list: "workbench-runs" });
    const datasetMore = button("More approved suites", () => loadDatasets().catch(err => notice(status, err)));
    const runsMore = button("More runs", () => loadRuns().catch(err => notice(status, err)));
    async function loadDatasets() {
      const page = await api("/v1alpha1/dataset-versions", { project: scope, state: "DATASET_VERSION_STATE_APPROVED", page_size: 50, page_token: datasetCursor });
      if (!active()) return;
      for (const d of page.datasets || []) {
        const c = d.content, key = `${c.name}/${c.version}`;
        if (!suiteOptions.has(key)) { suiteOptions.set(key, d); dataset.append(el("option", { value: key }, `${c.name}@${c.version} · ${c.split || "unspecified split"}`)); }
      }
      datasetCursor = page.nextPageToken || ""; datasetMore.disabled = !datasetCursor;
      const requested = query.get("dataset");
      if (requested && suiteOptions.has(requested)) dataset.value = requested;
    }
    async function loadRuns() {
      const page = await api("/v1alpha1/runs", { project: scope, page_size: 50, page_token: runCursor });
      if (!active()) return;
      for (const r of page.runs || []) if (!r.imported && !r.archivedAt && ["RUN_STATUS_SUCCEEDED", "RUN_STATUS_FAILED"].includes(r.status)) {
        runOptions.append(el("option", { value: r.id }, `${r.name || r.id} · ${r.labels?.["agent.commit"] || "commit unavailable"}`));
      }
      runCursor = page.nextPageToken || ""; runsMore.disabled = !runCursor;
    }
    const mode = select([["recorded", "Score recorded outputs"], ["model", "Generate responses from a model endpoint"], ["http", "Run an HTTP agent"]]);
    const runName = input("candidate", { required: true }), commit = input("", { required: true, pattern: "[a-fA-F0-9]{7,64}" });
    const endpoint = input("", { type: "url" }), model = input();
    const keyEnv = select([["none", "No credential"], ...(credentials.grants || []).map(g => [g.env, `${g.env} · ${(g.hosts || []).join(", ") || "granted hosts"}`])]);
    if (!credentials.enforced) { keyEnv.append(el("option", { value: "OPENAI_API_KEY" }, "OPENAI_API_KEY")); }
    const judge = select([["", "Server default (if configured)"], ...(credentials.judges || []).map(j => [j, j])]);
    const trials = number(1, { min: 1, step: 1, required: true });
    const targetTokens = number("", { min: 0, step: 1 }), judgeTokens = number("", { min: 0, step: 1 });
    const outputPath = input("output"), trajectoryPath = input("trajectory"), usagePath = input("usage"), stopPath = input("stop_reason"), timeout = number(120, { min: 1, required: true });
    const body = input('{"input": {{input}}, "trial": {{trial}}, "session_id": {{session_id}}}');
    const header = input("Authorization");
    const endpointFields = el("div", {}, field("Endpoint URL", endpoint), field("Model name", model), field("Credential variable", keyEnv));
    const httpFields = el("details", {}, el("summary", {}, "HTTP agent mapping"), field("Credential header", header), field("Request body template", body), field("Answer path", outputPath), field("Trajectory path", trajectoryPath), field("Usage path", usagePath), field("Stop reason path", stopPath), field("HTTP timeout (seconds)", timeout));
    function modeChanged() {
      endpointFields.hidden = mode.value === "recorded"; httpFields.hidden = mode.value !== "http";
      endpoint.required = mode.value !== "recorded"; model.disabled = mode.value !== "model"; model.required = mode.value === "model";
      for (const node of httpFields.querySelectorAll("input")) node.disabled = mode.value !== "http";
    }
    mode.onchange = modeChanged; modeChanged();
    function resolveEvaluator(ref) {
      const name = ref.split("@")[0];
      const matches = catalog.evaluators.filter(m => name.includes("/") ? m.name === name : m.name.split("/").at(-1) === name);
      return matches.length === 1 ? matches[0] : undefined;
    }
    const evaluatorSelect = select((catalog.evaluators || []).map(m => [m.name, `${m.name}@${m.version}`]));
    const evaluatorCards = el("div"), evaluators = [];
    function addEvaluator(ref = "builtin/exact-match", params = {}, name = "") {
      const m = resolveEvaluator(ref);
      if (!m) throw new Error(`Evaluator ${ref} is unavailable.`);
      if (ref.includes("@") && ref.split("@")[1] !== m.version) throw new Error(`The pinned evaluator ${ref} is no longer available.`);
      if (Object.keys(params).some(k => !Object.hasOwn(m.paramsSchema?.properties || {}, k))) throw new Error(`Use the original spec for parameters outside ${m.name}'s current schema.`);
      const alias = input(name), controls = parameterControls(m.paramsSchema, params);
      const item = { read: () => ({ ref: `${m.name}@${m.version}`, ...(alias.value ? { name: alias.value } : {}), params: controls.read() }) };
      const card = el("div", { class: "card" }, el("h3", {}, m.name), el("p", { class: "sub" }, m.description || ""), m.requires?.judge ? el("p", {}, "Uses the explicitly configured judge; missing judge configuration fails admission.") : null,
        field("Evaluator alias (optional)", alias), controls.node, button("Remove evaluator", () => { evaluators.splice(evaluators.indexOf(item), 1); card.remove(); invalidateCandidate(); }));
      evaluators.push(item); evaluatorCards.append(card);
    }
    addEvaluator();
    let validated = "", submissionKey = attemptKey(), inspected = null, policyPin = "", reviewedSource = "", evidenceEpoch = 0;
    const preview = el("div"), runReview = input("", { type: "checkbox" });
    const start = el("button", { type: "submit", disabled: true }, "Start candidate run");
    function invalidateCandidate() { validated = ""; runReview.checked = false; start.disabled = true; preview.replaceChildren(); }
    function suiteRef() {
      const d = suiteOptions.get(dataset.value);
      if (!d) throw new Error("Select an approved dataset version.");
      return { project: d.content.project || scope, name: d.content.name, version: d.content.version, sha256: d.sha256 };
    }
    function candidateRequest() {
      const spec = { dataset: { catalog: suiteRef() }, evaluators: evaluators.map(e => e.read()), trials: Number(trials.value) };
      if (!spec.evaluators.length) throw new Error("Select at least one evaluator.");
      if (judge.value) spec.judge = judge.value;
      if (targetTokens.value || judgeTokens.value) spec.budget = { maxTargetTokens: targetTokens.value || "0", maxJudgeTokens: judgeTokens.value || "0" };
      if (mode.value === "model") spec.target = { connector: "openai-compatible", baseUrl: endpoint.value, model: model.value, apiKeyEnv: keyEnv.value };
      if (mode.value === "http") spec.target = { agent: { http: { url: endpoint.value, outputPath: outputPath.value, bodyTemplate: body.value, trajectoryPath: trajectoryPath.value, usagePath: usagePath.value, stopReasonPath: stopPath.value, timeout: `${timeout.value}s`, headersEnv: keyEnv.value === "none" ? {} : { [header.value]: keyEnv.value } } } };
      return { name: runName.value, project: scope, labels: { "agent.commit": commit.value }, spec };
    }
    const candidateForm = el("form", { onsubmit: async event => {
      event.preventDefault();
      if (start.disabled) return;
      start.disabled = true;
      candidateStatus.replaceChildren(el("p", {}, "Submitting candidate…"));
      try {
        const request = candidateRequest(), key = submissionKey;
        if (json(request) !== validated || !runReview.checked) throw new Error("Validate and review the current configuration before execution.");
        const response = await api("/v1alpha1/runs", {}, { method: "POST", body: JSON.stringify(request), headers: { "Content-Type": "application/json", "Idempotency-Key": key } });
        if (!active() || submissionKey !== key) return;
        candidate.value = response.run.id; resetEvidence();
        // A retry of this unchanged configuration reuses its admission key.
        candidateStatus.replaceChildren(el("p", {}, "Candidate submitted: ", el("a", { href: `#/runs/${encodeURIComponent(response.run.id)}` }, response.run.id)));
        pollCandidate(response.run.id);
      } catch (err) { if (active()) notice(candidateStatus, err); }
      finally { if (active()) start.disabled = !validated; }
    } });
    candidateForm.addEventListener("input", invalidateCandidate);
    candidateForm.addEventListener("change", invalidateCandidate);
    // Review checkbox changes do not alter the reviewed configuration.
    runReview.addEventListener("input", event => event.stopPropagation());
    runReview.addEventListener("change", event => event.stopPropagation());
    const validate = button("Validate candidate configuration", async () => {
      if (!candidateForm.reportValidity()) return;
      try {
        const request = candidateRequest(), source = json(request);
        invalidateCandidate();
        await post("/v1alpha1/runs", { ...request, validateOnly: true });
        if (!active() || source !== json(candidateRequest())) return;
        validated = source; start.disabled = false;
        preview.replaceChildren(el("p", {}, "Admission checks passed. Review the complete configuration and outbound destination before execution."), pre(request));
      } catch (err) { if (active()) notice(preview, err); }
    });
    const newAttempt = button("Begin a new explicit attempt", () => { submissionKey = attemptKey(); invalidateCandidate(); candidateStatus.replaceChildren(el("p", {}, "New attempt selected. Validate the configuration again.")); });
    function pollCandidate(id) {
      const poll = async () => {
        if (!active() || candidate.value !== id) return;
        try {
          const { run } = await api(`/v1alpha1/runs/${encodeURIComponent(id)}`);
          if (!active() || candidate.value !== id) return;
          candidateStatus.replaceChildren(el("p", {}, el("a", { href: `#/runs/${encodeURIComponent(id)}` }, id), ` · ${(run.status || "pending").replace("RUN_STATUS_", "")} · ${run.progress?.done || 0}/${run.progress?.total || 0}`), run.error ? el("p", { class: "error" }, run.error) : null);
          if (["RUN_STATUS_PENDING", "RUN_STATUS_RUNNING"].includes(run.status)) setTimeout(poll, 1500);
        } catch (err) { if (active()) notice(candidateStatus, err); }
      }; poll();
    }
    candidateForm.append(field("Approved dataset", dataset), datasetMore, field("Run name", runName), field("Declared candidate commit", commit), field("Run mode", mode), endpointFields, httpFields,
      el("h3", {}, "Evaluators"), field("Add evaluator", evaluatorSelect), button("Add selected evaluator", () => { addEvaluator(evaluatorSelect.value); invalidateCandidate(); }), evaluatorCards,
      field("Judge", judge), field("Trials per record", trials), field("Maximum target tokens (optional)", targetTokens), field("Maximum judge tokens (optional)", judgeTokens),
      el("p", { class: "sub" }, "All records in the approved suite are executed. Model and HTTP modes send inputs to the configured endpoint using worker credentials. Commit labels are declarations, not attestations."), validate, preview,
      field("I reviewed the candidate configuration and destination", runReview), start, newAttempt, candidateStatus);

    const identitySlot = el("div"), policySlot = el("div"), decisionSlot = el("div"), reviewed = input("", { type: "checkbox" });
    const policyName = input("production", { required: true }), policyVersion = input("v1", { required: true }), decisionID = input("", { required: true });
    const minRecords = number(30, { min: 2, step: 1, required: true }), confidence = select([["0.95", "95%"], ["0.9", "90%"], ["0.99", "99%"]]);
    const ruleCards = el("div"), rules = [], metricOptions = new Map();
    const metric = select([["", "Inspect runs to choose a metric"]]);
    function resetPolicy() { policyPin = ""; reviewedSource = ""; reviewed.checked = false; policySlot.replaceChildren(); exports.replaceChildren(); }
    function resetEvidence() { evidenceEpoch += 1; inspected = null; resetPolicy(); identitySlot.replaceChildren(); }
    for (const control of [baseline, candidate]) control.addEventListener("input", resetEvidence);
    const cloneBaseline = button("Use baseline evaluation settings", async () => {
      try {
        const requested = baseline.value;
        const { run } = await api(`/v1alpha1/runs/${encodeURIComponent(requested)}`);
        if (!active() || baseline.value !== requested) return;
        const spec = run.spec, ref = spec?.dataset?.catalog;
        if (!ref) throw new Error("The baseline must use an approved catalog suite.");
        const d = await api(`/v1alpha1/dataset-versions/${encodeURIComponent(ref.name)}/${encodeURIComponent(ref.version)}`, { project: ref.project || run.project });
        if (!active() || baseline.value !== requested) return;
        if (d.dataset.state !== "DATASET_VERSION_STATE_APPROVED" || d.dataset.sha256 !== ref.sha256) throw new Error("The baseline suite is retired, unapproved or changed.");
        const supported = Object.keys(spec).every(k => ["dataset", "evaluators", "judge", "trials", "budget", "target"].includes(k)) && Object.keys(spec.dataset).every(k => k === "catalog") && !spec.harness && !spec.environment && !spec.gates?.length && !spec.summary && (!spec.target || spec.target.agent?.http || spec.target.connector === "openai-compatible");
        if (!supported) throw new Error("This baseline has advanced harness, checker, gate or summary settings. Configure it through its original spec; the workbench will not silently drop those settings.");
        const target = spec.target;
        if (target?.agent) {
          const h = target.agent.http;
          if (!h || Object.keys(target).some(k => k !== "agent") || Object.keys(target.agent).some(k => k !== "http") || Object.keys(h).some(k => !["url", "method", "headersEnv", "bodyTemplate", "outputPath", "timeout", "usagePath", "trajectoryPath", "stopReasonPath"].includes(k)) || h.method && h.method !== "POST" || Object.keys(h.headersEnv || {}).length > 1) throw new Error("Use the original spec for this advanced HTTP baseline.");
        } else if (target && Object.keys(target).some(k => !["connector", "baseUrl", "model", "apiKeyEnv"].includes(k))) throw new Error("Use the original spec for advanced model settings.");
        if (spec.judge && ![...judge.options].some(o => o.value === spec.judge)) throw new Error("The baseline judge is not permitted for this project.");
        const credential = target?.agent?.http ? Object.values(target.agent.http.headersEnv || {})[0] || "none" : target ? target.apiKeyEnv || "OPENAI_API_KEY" : "none";
        if (![...keyEnv.options].some(o => o.value === credential)) throw new Error("The baseline credential variable is not available in this project's grants.");
        for (const e of spec.evaluators || []) {
          const manifest = resolveEvaluator(e.ref);
          if (!manifest || e.ref.includes("@") && e.ref.split("@")[1] !== manifest.version || Object.keys(e.params || {}).some(k => !Object.hasOwn(manifest.paramsSchema?.properties || {}, k))) throw new Error("A baseline evaluator or its parameter schema changed; use its original spec.");
        }
        const key = `${ref.name}/${ref.version}`;
        suiteOptions.set(key, d.dataset); if (![...dataset.options].some(o => o.value === key)) dataset.append(el("option", { value: key }, `${ref.project || run.project}/${ref.name}@${ref.version}`)); dataset.value = key;
        evaluators.splice(0); evaluatorCards.replaceChildren();
        for (const e of spec.evaluators || []) addEvaluator(e.ref, e.params, e.name);
        judge.value = spec.judge || ""; trials.value = spec.trials || 1;
        targetTokens.value = spec.budget?.maxTargetTokens || ""; judgeTokens.value = spec.budget?.maxJudgeTokens || "";
        const t = spec.target;
        if (t?.agent?.http) {
          const h = t.agent.http;
          // Only connector fields exposed below can be copied faithfully.
          if (h.stream || h.method && h.method !== "POST" || h.messagesPath || h.deltaPath || Object.keys(h.headersEnv || {}).length > 1) throw new Error("Use the original spec for this advanced HTTP baseline.");
          mode.value = "http"; endpoint.value = h.url; outputPath.value = h.outputPath || ""; body.value = h.bodyTemplate || '{"input": {{input}}}'; trajectoryPath.value = h.trajectoryPath || ""; usagePath.value = h.usagePath || ""; stopPath.value = h.stopReasonPath || ""; timeout.value = Number((h.timeout || "120s").slice(0, -1));
          const entries = Object.entries(h.headersEnv || {}); header.value = entries[0]?.[0] || "Authorization"; keyEnv.value = entries[0]?.[1] || "none";
        } else if (t) {
          if (Object.keys(t).some(k => !["connector", "baseUrl", "model", "apiKeyEnv"].includes(k))) throw new Error("Use the original spec for advanced model settings.");
          mode.value = "model"; endpoint.value = t.baseUrl || ""; model.value = t.model || ""; keyEnv.value = t.apiKeyEnv || "OPENAI_API_KEY";
        } else mode.value = "recorded";
        modeChanged(); invalidateCandidate();
        status.replaceChildren(el("p", {}, "Baseline settings copied. Set the candidate destination and commit, then validate."));
      } catch (err) { if (active()) notice(status, err); }
    });
    function addRule() {
      if (!metric.value) throw new Error("Inspect run identities and choose a measured metric first.");
      const chosen = metric.value, direction = select([["METRIC_DIRECTION_HIGHER", "Higher is better"], ["METRIC_DIRECTION_LOWER", "Lower is better"]]);
      direction.value = metricOptions.get(chosen)?.lower ? "METRIC_DIRECTION_LOWER" : "METRIC_DIRECTION_HIGHER";
      const bounds = Object.fromEntries(["maxRegression", "minMean", "maxMean", "minCase", "maxCase"].map(k => [k, number()]));
      const cases = el("textarea", { rows: 2, placeholder: "One record ID per line; empty applies to all cases" });
      const item = { read: () => ({ metric: chosen, direction: direction.value, ...Object.fromEntries(Object.entries(bounds).filter(([, node]) => node.value !== "").map(([key, node]) => [key, Number(node.value)])), caseIds: cases.value.split("\n").map(s => s.trim()).filter(Boolean) }) };
      const card = el("div", { class: "card" }, el("h3", {}, chosen), field("Metric direction", direction),
        ...Object.entries({ maxRegression: "Maximum paired regression", minMean: "Minimum candidate mean", maxMean: "Maximum candidate mean", minCase: "Minimum per-trial critical score", maxCase: "Maximum per-trial critical score" }).map(([key, label]) => field(label, bounds[key])), field("Critical case IDs", cases), button("Remove rule", () => { rules.splice(rules.indexOf(item), 1); card.remove(); resetPolicy(); }));
      rules.push(item); ruleCards.append(card); resetPolicy();
    }
    function policyValue() {
      if (!inspected) throw new Error("Inspect both run identities first.");
      if (!rules.length) throw new Error("Add at least one metric rule.");
      return { name: policyName.value, version: policyVersion.value, dataset: inspected[1].evidence.dataset, minRecords: Number(minRecords.value), confidenceLevel: Number(confidence.value), rules: rules.map(r => r.read()) };
    }
    const inspectRuns = button("Inspect baseline and candidate", async () => {
      try {
        resetEvidence(); const epoch = evidenceEpoch, ids = [baseline.value, candidate.value];
        if (ids.some(id => !id)) throw new Error("Explicit baseline and candidate run IDs are required.");
        const responses = await Promise.all(ids.map(async id => ({ ...(await api(`/v1alpha1/runs/${encodeURIComponent(id)}/release-evidence`)), ...(await api(`/v1alpha1/runs/${encodeURIComponent(id)}`)) })));
        if (!active() || evidenceEpoch !== epoch || ids[0] !== baseline.value || ids[1] !== candidate.value) return;
        inspected = responses;
        identitySlot.replaceChildren(...responses.map((v, i) => el("div", { class: "card" }, el("h3", {}, i === 0 ? "Baseline identity" : "Candidate identity"), pre(v.evidence))));
        metricOptions.clear();
        for (const summary of responses[1].run.summaries || []) {
          if (Number(summary.n) <= 0 || summary.mean === undefined) continue;
          const instance = responses[1].run.spec.evaluators.find(e => (e.name || e.ref.split("@")[0].split("/").at(-1)) === summary.evaluator);
          const manifest = instance ? resolveEvaluator(instance.ref) : undefined;
          const output = manifest?.outputs?.find(o => summary.metric === o.name || summary.metric.endsWith("." + o.name) || manifest.outputs.length === 1);
          metricOptions.set(summary.metric, { lower: output?.higherIsBetter === false });
        }
        const usage = responses[1].run.targetUsage;
        if (usage?.costUsd !== undefined) metricOptions.set("usage.cost_usd", { lower: true });
        // Latency is stored on per-record usage, not necessarily the run total.
        // Only offer it after finding a measured record in a bounded sample.
        const sample = await api(`/v1alpha1/runs/${encodeURIComponent(ids[1])}/results`, { page_size: 1 });
        if (!active() || inspected !== responses) return;
        const rec = sample.recordsByTrial?.[0]?.record || sample.records?.[0];
        if (rec?.usage?.latency !== undefined) metricOptions.set("usage.latency_ms", { lower: true });
        if (rec?.usage?.costUsd !== undefined) metricOptions.set("usage.cost_usd", { lower: true });
        metric.replaceChildren(el("option", { value: "" }, "Select a measured metric"), ...[...metricOptions.keys()].map(k => el("option", { value: k }, k)));
      } catch (err) { if (active()) { resetEvidence(); notice(identitySlot, err); } }
    });
    const exports = el("div");
    function exportControls() {
      if (!inspected || !policyPin || !reviewed.checked || reviewedSource !== json(policyValue())) throw new Error("Review the current policy and both run identities first.");
      const base = inspected[0].evidence.reference, run = inspected[1].run;
      const spec = { apiVersion: "evals.si/v1alpha1", kind: "EvalRun", metadata: { name: run.name, project: run.project, labels: run.labels }, spec: run.spec };
      const sh = s => "'" + String(s).replaceAll("'", "'\\''") + "'";
      const args = `--server ${sh(location.origin)} --project ${sh(scope)} --baseline ${sh(base.runId)} --baseline-sha256 ${sh(base.sha256)} --baseline-commit ${sh(base.commit)} --policy-sha256 ${sh(policyPin)}`;
      const commands = `# Requires matching unreleased SDK/server/worker builds. Set credentials in your environment.\n# This executes a NEW full-suite attempt; review the candidate deployment first.\nevalsi release gate -f release-policy.json ${args} --spec candidate.json --commit "$CANDIDATE_COMMIT" --id "$NEW_ATTEMPT_ID" -o new-attempt.release.json\n`;
      const action = `# Pin the action to a reviewed matching source commit and provision matching packages.\n- uses: Abhishek-Rnjn/Evals.si@REPLACE_WITH_MATCHING_SOURCE_COMMIT\n  with:\n    install: 'false'\n    spec: candidate.json\n    server: ${json(location.origin)}\n    release-policy: release-policy.json\n    release-policy-sha256: ${json(policyPin)}\n    baseline-run: ${json(base.runId)}\n    baseline-sha256: ${json(base.sha256)}\n    baseline-commit: ${json(base.commit)}\n  env:\n    EVALSI_API_KEY: \${{ secrets.EVALSI_API_KEY }}\n`;
      return el("div", {}, el("p", { class: "sub" }, "Exports can contain private configuration. Credentials are variable references only. CLI execution creates a new full-suite attempt; remote endpoint behavior is not deterministic replay. Replace the Action source placeholder with a reviewed matching commit."),
        button("Download candidate spec", () => download("candidate.json", json(spec))), button("Download reviewed release policy", () => download("release-policy.json", json(policyValue()))), button("Download CLI commands", () => download("release-check.sh", commands)), button("Download Action step", () => download("release-check.action.yaml", action)));
    }
    const policyForm = el("form", { onsubmit: async event => {
      event.preventDefault();
      try {
        if (!policyPin || !reviewed.checked || reviewedSource !== json(policyValue())) throw new Error("Inspect and review the current policy and identities first.");
        const pinned = inspected;
        const response = await post("/v1alpha1/release-decisions", { project: scope, id: decisionID.value, policy: policyValue(), policySha256: policyPin, baseline: pinned[0].evidence.reference, candidate: pinned[1].evidence.reference });
        if (active()) location.hash = `#/releases/${encodeURIComponent(response.decision.id)}`;
      } catch (err) { if (active()) notice(decisionSlot, err); }
    } });
    policyForm.addEventListener("input", resetPolicy); policyForm.addEventListener("change", resetPolicy);
    for (const node of [reviewed, decisionID]) { node.addEventListener("input", event => event.stopPropagation()); node.addEventListener("change", event => event.stopPropagation()); }
    reviewed.onchange = () => { exports.replaceChildren(); if (reviewed.checked && policyPin) exports.append(exportControls()); };
    policyForm.append(field("Policy name", policyName), field("Policy version", policyVersion), field("Minimum independent records", minRecords), field("Confidence level", confidence), field("Metric", metric), button("Add metric rule", () => { try { addRule(); } catch (err) { notice(policySlot, err); } }), ruleCards,
      el("p", { class: "sub" }, "Maximum regression checks the upper paired confidence bound. Critical-case bounds apply to every trial. Errors, skips, missing usage and incomplete coverage cannot silently pass. Trials do not increase the independent record count."),
      button("Inspect configured release policy", async () => {
        try {
          // The decision ID is not part of policy inspection or its digest.
          decisionID.required = false;
          const valid = policyForm.reportValidity(); decisionID.required = true;
          if (!valid) return;
          const source = json(policyValue()), pins = inspected; resetPolicy();
          const response = await post("/v1alpha1/release-policies:inspect", { policy: JSON.parse(source) });
          if (!active() || inspected !== pins || source !== json(policyValue())) return;
          policyPin = response.sha256; reviewedSource = source;
          policySlot.replaceChildren(el("p", {}, "Policy SHA-256: ", el("code", {}, policyPin)), pre(JSON.parse(source)));
        } catch (err) { if (active()) notice(policySlot, err); }
      }), policySlot, field("I reviewed the configured policy and both run identities", reviewed), field("New immutable release decision ID", decisionID),
      el("button", { type: "submit" }, "Evaluate configured release check"), decisionSlot, exports);
    show(el("h1", {}, "Release workbench"), kv([["Project", scope]]), el("p", { class: "sub" }, "Configure a full regression run, compare explicit evidence, and inspect a release decision. Decisions do not deploy software."),
      el("h2", {}, "1. Select an accepted baseline"), field("Accepted baseline run", baseline), runOptions, runsMore, cloneBaseline, status,
      el("h2", {}, "2. Configure a candidate"), candidateForm,
      el("h2", {}, "3. Inspect release evidence"), field("Candidate run ID", candidate), inspectRuns, identitySlot,
      el("h2", {}, "4. Configure the release check"), policyForm);
    await Promise.all([loadDatasets(), loadRuns()]);
    if (candidate.value) pollCandidate(candidate.value);
  }
  return { view, inspect, recordPanel, caseLink, workbenchLink, exactKey };
};
