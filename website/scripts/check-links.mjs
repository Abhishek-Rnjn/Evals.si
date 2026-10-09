// Fails if any internal href/src in dist/ points at a missing file.
import { promises as fs } from "node:fs";
import path from "node:path";
const dist = path.resolve("dist");
const base = (process.env.SITE_BASE ?? "/Evals.si").replace(/\/$/, "");
const walk = async (d) => (await Promise.all((await fs.readdir(d, { withFileTypes: true })).map((e) => e.isDirectory() ? walk(path.join(d, e.name)) : [path.join(d, e.name)]))).flat();
const bad = new Set();
for (const f of (await walk(dist)).filter((f) => f.endsWith(".html"))) {
  const html = await fs.readFile(f, "utf8");
  for (const [, u] of html.matchAll(/(?:href|src)="([^"#?]+)[^"]*"/g)) {
    if (!u.startsWith(base + "/") && u !== base) { if (u.startsWith("/")) bad.add(`${path.relative(dist, f)} -> ${u} (outside base)`); continue; }
    let p = path.join(dist, u.slice(base.length));
    const ok = await fs.stat(p).then((s) => s.isFile() || fs.stat(path.join(p, "index.html")).then(() => true, () => false), () => false);
    if (!ok) bad.add(`${path.relative(dist, f)} -> ${u}`);
  }
}
if (bad.size) { console.error([...bad].join("\n")); process.exit(1); }
console.log("links ok");
