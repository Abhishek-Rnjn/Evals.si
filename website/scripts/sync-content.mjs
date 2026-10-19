// Builds src/content/docs from the repository's own docs, so the repo stays
// the single source of truth. Run by `npm run dev` and `npm run build`.
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, "../..");
const out = path.resolve(here, "../src/content/docs");
const BASE = (process.env.SITE_BASE ?? "/").replace(/\/$/, "");
const REPO = process.env.REPO_URL ?? "https://github.com/Abhishek-Rnjn/Evals.si";
const BRANCH = process.env.REPO_BRANCH ?? "main";

const read = (p) => fs.readFile(path.join(root, p), "utf8");
const exists = (p) => fs.stat(path.join(root, p)).then(() => true, () => false);

// ---- page registry: repo path -> site route -------------------------------
const pages = new Map(); // repo-relative source path -> route (no base)
const queue = []; // {src, route, title?, description?, text}

const guides = [
  ["agent-runs", 1], ["annotation", 2], ["fine-tuning", 3], ["guardrails", 4],
  ["identity", 5], ["kubernetes", 6], ["agentgateway-kubernetes", 7],
  ["mcp", 8], ["plugins", 9], ["web-ui", 10], ["webhooks", 11], ["trace-sources", 12], ["integrate-an-agent-harness", 13],
  ["agent-frameworks", 14], ["operations", 15], ["end-to-end", 16], ["deepeval", 17], ["gate-an-agent-in-ci", 18], ["evaluate-rag", 19], ["onboarding", 20], ["harness2", 21],
];
for (const [name] of guides) pages.set(`docs/guides/${name}.md`, `/docs/guides/${name}/`);
pages.set("docs/DESIGN.md", "/docs/architecture/");
pages.set("docs/LEFTOVERS.md", "/docs/roadmap/");
pages.set("docs/PRODUCT-ROADMAP.md", "/docs/product-roadmap/");
pages.set("docs/PRODUCT-STRATEGY.md", "/docs/product-strategy/");
pages.set("docs/decisions/README.md", "/docs/decisions/");
const decisions = (await fs.readdir(path.join(root, "docs/decisions"))).filter((f) => /^\d{4}-.*\.md$/.test(f)).sort();
for (const f of decisions) pages.set(`docs/decisions/${f}`, `/docs/decisions/${f.replace(/\.md$/, "")}/`);
pages.set("README.md", "/docs/get-started/");
pages.set("CHANGELOG.md", "/docs/changelog/");

// ---- link rewriting ---------------------------------------------------------
const siteHref = (route, frag) => `${BASE}${route}${frag ? "#" + frag : ""}`;
async function rewriteLinks(text, src) {
  const dir = path.posix.dirname(src);
  const re = /\]\(([^)\s]+)\)/g;
  const jobs = [];
  text.replace(re, (m, target) => { jobs.push(target); return m; });
  const map = new Map();
  for (const target of new Set(jobs)) {
    if (/^(https?:|mailto:|#)/.test(target)) continue;
    const [p, frag] = target.split("#");
    const resolved = path.posix.normalize(path.posix.join(dir, p));
    if (pages.has(resolved)) { map.set(target, siteHref(pages.get(resolved), frag)); continue; }
    if (resolved.startsWith("..")) continue;
    const isDir = (await fs.stat(path.join(root, resolved)).catch(() => null))?.isDirectory();
    const kind = isDir || p.endsWith("/") ? "tree" : "blob";
    map.set(target, `${REPO}/${kind}/${BRANCH}/${resolved.replace(/\/$/, "")}${frag ? "#" + frag : ""}`);
  }
  return text.replace(re, (m, target) => (map.has(target) ? `](${map.get(target)})` : m));
}

const frontmatter = (fm) =>
  "---\n" + Object.entries(fm).map(([k, v]) => `${k}: ${typeof v === "object" ? JSON.stringify(v) : JSON.stringify(v)}`).join("\n") + "\n---\n\n";

async function writePage(route, fm, body) {
  const file = path.join(out, route.replace(/^\//, ""), "index.md");
  await fs.mkdir(path.dirname(file), { recursive: true });
  await fs.writeFile(file, frontmatter(fm) + body);
}

// The page's meta description, for search results and link previews: its
// first paragraph of prose as plain text, cut at a word near 160 characters.
// Without one every page would share the site's description.
function describe(text) {
  for (const block of text.split(/\n\s*\n/)) {
    const b = block.trim();
    if (!b || /^(#|\||```|<|>|!\[|- |\* |\d+\. |---|Status:)/.test(b)) continue;
    let plain = b
      .replace(/!\[[^\]]*\]\([^)]*\)/g, "")
      .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
      .replace(/[`*_]/g, "")
      .replace(/\s+/g, " ")
      .trim();
    // A paragraph that leads into a list ("It shows:") loses that last
    // sentence, or ends with a full stop when it is the only one.
    if (plain.endsWith(":")) {
      const head = plain.replace(/\s*[^.!?]*:$/, "");
      plain = head.length >= 40 ? head : plain.slice(0, -1) + ".";
    }
    if (plain.length < 40) continue;
    return plain.length <= 160 ? plain : plain.slice(0, 157).replace(/\s+\S*$/, "") + "…";
  }
  return undefined;
}

// Take a repo markdown file, lift its H1 into the title and rewrite its links.
async function importDoc(src, route, extra = {}) {
  let text = await read(src);
  const m = text.match(/^# (.+)\n/);
  const title = (extra.title ?? m?.[1] ?? path.basename(src, ".md")).replace(/`/g, "");
  if (m) text = text.slice(m[0].length);
  const description = extra.description ?? describe(text);
  text = await rewriteLinks(text, src);
  const edit = `${REPO}/edit/${BRANCH}/${src}`;
  await writePage(route, { title, editUrl: edit, ...extra, ...(description ? { description } : {}), title }, text);
}

await fs.rm(out, { recursive: true, force: true });
await fs.mkdir(out, { recursive: true });

// Guides, architecture, roadmap, decisions
for (const [name] of guides) await importDoc(`docs/guides/${name}.md`, `/docs/guides/${name}/`);
await importDoc("docs/DESIGN.md", "/docs/architecture/", { title: "Architecture and implementation plan" });
await importDoc("docs/LEFTOVERS.md", "/docs/roadmap/", { title: "Roadmap and open leftovers" });
await importDoc("docs/PRODUCT-ROADMAP.md", "/docs/product-roadmap/", { title: "Product roadmap", description: "Proposed work for production-agent and enterprise AI teams, with priorities, dependencies and acceptance criteria." });
await importDoc("docs/PRODUCT-STRATEGY.md", "/docs/product-strategy/", { title: "Product strategy", description: "Competitive research and proposed adoption, deployment and governance strategy for Evals.si." });
await importDoc("docs/decisions/README.md", "/docs/decisions/", { title: "Decision records" });
for (const f of decisions) await importDoc(`docs/decisions/${f}`, `/docs/decisions/${f.replace(/\.md$/, "")}/`);
await importDoc("CHANGELOG.md", "/docs/changelog/", { description: "What changed in each Evals.si release." });

// Get started: the README's install options and quickstart, then its tour of the features
{
  const readme = await read("README.md");
  const section = (h) => {
    const start = readme.indexOf(`\n## ${h}\n`);
    if (start === -1) throw new Error(`README.md has no "## ${h}" section`);
    const next = readme.indexOf("\n## ", start + 4);
    return readme.slice(start + `\n## ${h}\n`.length, next === -1 ? undefined : next);
  };
  const body = "\n## Install\n" + section("Install") + "\n## Quickstart\n" + section("Quickstart") + "\n## A tour\n" + section("A tour");
  const intro = "Install, grade your first outputs, run evaluators from Python or as a server, then take a tour of runs, agents and online evaluation.\n";
  let fixed = await rewriteLinks(intro + body, "README.md");
  // In-page anchors that point at README sections not imported here go to GitHub.
  const slug = (h) => h.toLowerCase().replace(/[^\w\s-]/g, "").trim().replace(/\s/g, "-");
  const have = new Set([...fixed.matchAll(/^#{1,6} (.+)$/gm)].map((m) => slug(m[1])));
  fixed = fixed.replace(/\]\(#([^)\s]+)\)/g, (m, a) => (have.has(a) ? m : `](${REPO}/blob/${BRANCH}/README.md#${a})`));
  await writePage("/docs/get-started/", { title: "Get started", description: "Install Evals.si and run your first evaluation.", editUrl: `${REPO}/edit/${BRANCH}/README.md` }, fixed);
}

// Docs overview
await writePage("/docs/", { title: "Documentation", description: "Guides, architecture and reference for Evals.si." }, `
Evals.si evaluates AI before you ship, while it runs, and as you train it. It runs agents, RAG pipelines and models on datasets and sandboxed tasks and fails the build when a gate fails, scores live traces, and turns the same evaluators into RL rewards and checkpoint checks. Agents come first.

## Start here

- [Get started](${BASE}/docs/get-started/): install (PyPI, release binaries, charts) and run your first evaluation in a minute.
- [Changelog](${BASE}/docs/changelog/): what changed in each release.
- [Examples](${BASE}/examples/): runnable configs for runs, agents, guardrails, fine-tuning and more.
- [Agent frameworks](${BASE}/docs/guides/agent-frameworks/): score LangGraph, CrewAI, OpenAI Agents SDK, LlamaIndex and Claude Agent SDK traces.
- [Trace sources](${BASE}/docs/guides/trace-sources/): score the traces you already keep in MLflow, Phoenix or Langfuse.
- [Operations](${BASE}/docs/guides/operations/): upgrade policy, backup and restore, runbooks and reference cluster values.

## Guides

${guides.map(([n]) => `- [${n.replace(/-/g, " ")}](${BASE}/docs/guides/${n}/)`).join("\n")}

## Understand the design

- [Architecture and implementation plan](${BASE}/docs/architecture/)
- [Product roadmap](${BASE}/docs/product-roadmap/): proposed work, dependencies and acceptance criteria.
- [Product strategy](${BASE}/docs/product-strategy/): competitive research and adoption plan.
- [Decision records](${BASE}/docs/decisions/)
- [Roadmap and open leftovers](${BASE}/docs/roadmap/)
`);

// ---- examples ---------------------------------------------------------------
const examples = [
  { dir: "quickstart", title: "Quickstart: grade existing outputs", desc: "Score a JSONL file of question/answer records with built-in evaluators.", guide: "/docs/get-started/" },
  { dir: "runs", title: "Runs: execute a target on a dataset", desc: "A durable run with a dataset, evaluators and a pass/fail gate.", guide: "/docs/get-started/" },
  { dir: "agent-gate", title: "Gate an agent in CI", desc: "A runnable agent gate: a stand-in agent, a hidden checker and a pass/fail gate that exits 3, plus a GitHub Actions workflow.", guide: "/docs/guides/gate-an-agent-in-ci/" },
  { dir: "rag", title: "Evaluate a RAG pipeline", desc: "A gate over a RAG pipeline's recorded answers: question, answer, retrieved passages and a citation check, with no model needed.", guide: "/docs/guides/evaluate-rag/" },
  { dir: "agents", title: "Agent runs on sandboxed tasks", desc: "Put the built-in agent, or your own, to work on tasks in a sandbox, including SWE-bench.", guide: "/docs/guides/agent-runs/" },
  { dir: "watch", title: "Watch: evaluate live traces", desc: "An online policy that scores OpenTelemetry traces as they arrive.", guide: "/docs/get-started/" },
  { dir: "sources", title: "Trace sources: score traces kept in MLflow", desc: "A TraceSource that reads an agent harness's MLflow traces, scores them and writes the scores back.", guide: "/docs/guides/trace-sources/" },
  { dir: "guardrails", title: "Inline guardrails", desc: "Block or flag model output through agentgateway before it reaches users.", guide: "/docs/guides/guardrails/" },
  { dir: "annotation", title: "Human annotation queues", desc: "Collect human labels and measure agreement alongside automated scores.", guide: "/docs/guides/annotation/" },
  { dir: "finetuning", title: "Fine-tuning and RL rewards", desc: "Evaluators as RL rewards, and checkpoint checks for forgetting and safety regressions.", guide: "/docs/guides/fine-tuning/" },
  { dir: "custom-evaluator", title: "Custom Python evaluator", desc: "Write your own evaluator as a decorated function, test it, and install it as a pack so it runs by name.", guide: "/docs/guides/plugins/" },
  { dir: "wasm", title: "Wasm evaluator plugin", desc: "Write an evaluator in Go, compile it to Wasm and publish it through the plugin index.", guide: "/docs/guides/plugins/" },
  { dir: "auth", title: "Identity and access control", desc: "Server config with OIDC, API keys and project-scoped roles.", guide: "/docs/guides/identity/" },
  { dir: "server", title: "Server configuration", desc: "A baseline evalsid configuration.", guide: "/docs/get-started/" },
  { dir: "ci", title: "Gate pull requests in CI", desc: "A GitHub Actions workflow that fails a build when evaluation scores regress.", guide: "/docs/get-started/" },
];
const langOf = (f) => ({ ".yaml": "yaml", ".yml": "yaml", ".jsonl": "json", ".json": "json", ".go": "go", ".py": "python", ".md": "markdown" }[path.extname(f)] ?? "text");
async function filesIn(rel) {
  const abs = path.join(root, rel);
  const entries = await fs.readdir(abs, { withFileTypes: true });
  const files = [];
  for (const e of entries.sort((a, b) => a.name.localeCompare(b.name))) {
    if (e.name.startsWith(".")) continue;
    if (e.isDirectory()) files.push(...(await filesIn(`${rel}/${e.name}`)));
    else files.push(`${rel}/${e.name}`);
  }
  return files;
}
for (const ex of examples) {
  const files = await filesIn(`examples/${ex.dir}`);
  let body = `${ex.desc}\n\nSource: [\`examples/${ex.dir}\`](${REPO}/tree/${BRANCH}/examples/${ex.dir}). Read the [guide](${BASE}${ex.guide}) for the concepts behind it.\n`;
  for (const f of files) {
    const text = (await read(f)).trimEnd();
    const fence = "````";
    body += `\n## \`${f.replace(`examples/${ex.dir}/`, "")}\`\n\n${fence}${langOf(f)}\n${text}\n${fence}\n`;
  }
  await writePage(`/examples/${ex.dir}/`, { title: ex.title, description: ex.desc, editUrl: `${REPO}/tree/${BRANCH}/examples/${ex.dir}` }, body);
}
await writePage("/examples/", { title: "Examples", description: "Runnable configurations for every part of Evals.si." }, `
Every example lives under [\`examples/\`](${REPO}/tree/${BRANCH}/examples) in the repository and is shown here in full.

${examples.map((e) => `- [${e.title}](${BASE}/examples/${e.dir}/): ${e.desc}`).join("\n")}
`);

// ---- hand-written pages (website/content), with __BASE__ replaced ----------
async function copyHandwritten(dir) {
  const src = path.resolve(here, "../content", dir);
  for (const e of await fs.readdir(src, { withFileTypes: true }).catch(() => [])) {
    const rel = path.join(dir, e.name);
    if (e.isDirectory()) { await copyHandwritten(rel); continue; }
    const text = (await fs.readFile(path.join(src, e.name), "utf8")).replaceAll("__BASE__", BASE);
    const name = e.name === "index.md" ? "index.md" : path.join(e.name.replace(/\.md$/, ""), "index.md");
    const dest = path.join(out, dir, name);
    await fs.mkdir(path.dirname(dest), { recursive: true });
    await fs.writeFile(dest, text);
  }
}
await copyHandwritten("integrations");

console.log(`synced ${queue.length + pages.size + examples.length} pages into ${path.relative(process.cwd(), out)}`);
