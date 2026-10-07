// Builds src/content/docs from the repository's own docs, so the repo stays
// the single source of truth. Run by `npm run dev` and `npm run build`.
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, "../..");
const out = path.resolve(here, "../src/content/docs");
const BASE = (process.env.SITE_BASE ?? "/Evals.si").replace(/\/$/, "");
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
  ["mcp", 8], ["plugins", 9], ["web-ui", 10],
];
for (const [name] of guides) pages.set(`docs/guides/${name}.md`, `/docs/guides/${name}/`);
pages.set("docs/DESIGN.md", "/docs/architecture/");
pages.set("docs/LEFTOVERS.md", "/docs/roadmap/");
pages.set("docs/decisions/README.md", "/docs/decisions/");
const decisions = (await fs.readdir(path.join(root, "docs/decisions"))).filter((f) => /^\d{4}-.*\.md$/.test(f)).sort();
for (const f of decisions) pages.set(`docs/decisions/${f}`, `/docs/decisions/${f.replace(/\.md$/, "")}/`);
pages.set("README.md", "/docs/get-started/");

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

// Take a repo markdown file, lift its H1 into the title and rewrite its links.
async function importDoc(src, route, extra = {}) {
  let text = await read(src);
  const m = text.match(/^# (.+)\n/);
  const title = (extra.title ?? m?.[1] ?? path.basename(src, ".md")).replace(/`/g, "");
  if (m) text = text.slice(m[0].length);
  text = await rewriteLinks(text, src);
  const edit = `${REPO}/edit/${BRANCH}/${src}`;
  await writePage(route, { title, editUrl: edit, ...extra, title }, text);
}

await fs.rm(out, { recursive: true, force: true });
await fs.mkdir(out, { recursive: true });

// Guides, architecture, roadmap, decisions
for (const [name] of guides) await importDoc(`docs/guides/${name}.md`, `/docs/guides/${name}/`);
await importDoc("docs/DESIGN.md", "/docs/architecture/", { title: "Architecture and implementation plan" });
await importDoc("docs/LEFTOVERS.md", "/docs/roadmap/", { title: "Roadmap and open leftovers" });
await importDoc("docs/decisions/README.md", "/docs/decisions/", { title: "Decision records" });
for (const f of decisions) await importDoc(`docs/decisions/${f}`, `/docs/decisions/${f.replace(/\.md$/, "")}/`);

// Get started: the README quickstart through the end of its subsections
{
  const readme = await read("README.md");
  const start = readme.indexOf("## Quickstart");
  const next = readme.indexOf("\n## ", start + 5);
  const body = readme.slice(start + "## Quickstart".length, next === -1 ? undefined : next);
  const intro = "Install, grade your first outputs, then run evaluators from Python or as a server.\n";
  let fixed = await rewriteLinks(intro + body, "README.md");
  // In-page anchors that point at README sections not imported here go to GitHub.
  const slug = (h) => h.toLowerCase().replace(/[^\w\s-]/g, "").trim().replace(/\s/g, "-");
  const have = new Set([...fixed.matchAll(/^#{1,6} (.+)$/gm)].map((m) => slug(m[1])));
  fixed = fixed.replace(/\]\(#([^)\s]+)\)/g, (m, a) => (have.has(a) ? m : `](${REPO}/blob/${BRANCH}/README.md#${a})`));
  await writePage("/docs/get-started/", { title: "Get started", description: "Install Evals.si and run your first evaluation.", editUrl: `${REPO}/edit/${BRANCH}/README.md` }, fixed);
}

// Docs overview
await writePage("/docs/", { title: "Documentation", description: "Guides, architecture and reference for Evals.si." }, `
Evals.si is one entrypoint for evaluating classic ML models, LLMs, RAG systems and agents, offline and online.

## Start here

- [Get started](${BASE}/docs/get-started/): install and run your first evaluation in a minute.
- [Examples](${BASE}/examples/): runnable configs for runs, agents, guardrails, fine-tuning and more.

## Guides

${guides.map(([n]) => `- [${n.replace(/-/g, " ")}](${BASE}/docs/guides/${n}/)`).join("\n")}

## Understand the design

- [Architecture and implementation plan](${BASE}/docs/architecture/)
- [Decision records](${BASE}/docs/decisions/)
- [Roadmap and open leftovers](${BASE}/docs/roadmap/)
`);

// ---- examples ---------------------------------------------------------------
const examples = [
  { dir: "quickstart", title: "Quickstart: grade existing outputs", desc: "Score a JSONL file of question/answer records with built-in evaluators.", guide: "/docs/get-started/" },
  { dir: "runs", title: "Runs: execute a target on a dataset", desc: "A durable run with a dataset, evaluators and a pass/fail gate.", guide: "/docs/get-started/" },
  { dir: "agents", title: "Agent runs on sandboxed tasks", desc: "Put the built-in agent, or your own, to work on tasks in a sandbox, including SWE-bench.", guide: "/docs/guides/agent-runs/" },
  { dir: "watch", title: "Watch: evaluate live traces", desc: "An online policy that scores OpenTelemetry traces as they arrive.", guide: "/docs/get-started/" },
  { dir: "guardrails", title: "Inline guardrails", desc: "Block or flag model output through agentgateway before it reaches users.", guide: "/docs/guides/guardrails/" },
  { dir: "annotation", title: "Human annotation queues", desc: "Collect human labels and measure agreement alongside automated scores.", guide: "/docs/guides/annotation/" },
  { dir: "finetuning", title: "Fine-tuning and RL rewards", desc: "Evaluators as RL rewards, and checkpoint checks for forgetting and safety regressions.", guide: "/docs/guides/fine-tuning/" },
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

console.log(`synced ${queue.length + pages.size + examples.length} pages into ${path.relative(process.cwd(), out)}`);
