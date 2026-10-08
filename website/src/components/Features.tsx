import { Boxes, Cpu, KeyRound, Puzzle, ServerCog, ShieldCheck, Sparkles } from "lucide-react";
import { BentoCard, BentoGrid } from "./ui/bento-grid";
import { Marquee } from "./ui/marquee";


function SandboxLadder() {
  const rungs = [
    { name: "Firecracker microVM", level: "vm", on: true },
    { name: "bubblewrap", level: "namespaced", on: false },
    { name: "Landlock", level: "confined", on: false },
    { name: "Hardened pod", level: "k8s", on: false },
  ];
  return (
    <div className={`absolute right-4 top-6 w-[85%] space-y-2 sm:right-8 sm:w-[60%]`}>
      {rungs.map((r) => (
        <div key={r.name} className="flex items-center justify-between rounded-lg border border-neutral-200 bg-neutral-50 px-4 py-2.5 font-mono text-xs transition-transform duration-300 group-hover:translate-x-1 dark:border-white/10 dark:bg-white/[.03]">
          <span className="flex items-center gap-2">
            <span className={`size-2 rounded-full ${r.on ? "bg-teal-400 shadow-[0_0_8px_2px_rgba(45,212,191,0.6)]" : "bg-neutral-300 dark:bg-neutral-700"}`} />
            {r.name}
          </span>
          <span className="text-neutral-400">{r.level}</span>
        </div>
      ))}
      <div className="pt-1 text-right font-mono text-[11px] text-rose-400">no rung qualifies → refuse, never downgrade</div>
    </div>
  );
}

function Adapters() {
  const names = ["lm-eval-harness", "Inspect AI", "RAGAS", "DeepEval", "SWE-bench", "τ-bench", "Terminal-Bench", "BFCL", "Harbor", "Wasm plugins"];
  return (
    <Marquee vertical pauseOnHover repeat={3} className="absolute inset-x-0 top-0 h-[60%] [--duration:25s]">
      {names.map((n) => (
        <div key={n} className="mx-auto w-[80%] rounded-lg border border-neutral-200 bg-neutral-50 px-4 py-2 text-center font-mono text-xs dark:border-white/10 dark:bg-white/[.03]">{n}</div>
      ))}
    </Marquee>
  );
}

function Guardrail() {
  return (
    <div className={`absolute inset-x-6 top-6 space-y-3 font-mono text-xs`}>
      <div className="rounded-lg border border-neutral-200 bg-neutral-50 p-3 dark:border-white/10 dark:bg-white/[.03]">
        reach me at <span className="rounded bg-indigo-500/15 px-1 text-indigo-500 dark:text-indigo-300">&lt;EMAIL&gt;</span>, card <span className="rounded bg-indigo-500/15 px-1 text-indigo-500 dark:text-indigo-300">&lt;CARD&gt;</span>
      </div>
      <div className="flex gap-2">
        <span className="rounded-full bg-teal-500/15 px-3 py-1 text-teal-600 dark:text-teal-300">pass</span>
        <span className="rounded-full bg-amber-500/15 px-3 py-1 text-amber-600 dark:text-amber-300 ring-1 ring-amber-400/50">mask</span>
        <span className="rounded-full bg-rose-500/15 px-3 py-1 text-rose-600 dark:text-rose-300">block</span>
      </div>
    </div>
  );
}

function Crd() {
  return (
    <pre className={`absolute right-0 top-4 w-[90%] overflow-hidden rounded-l-xl border border-r-0 border-neutral-200 bg-neutral-50 p-4 font-mono text-[11.5px] leading-relaxed text-neutral-600 transition-transform duration-300 group-hover:-translate-x-2 sm:w-[62%] dark:border-white/10 dark:bg-white/[.03] dark:text-neutral-400`}>
{`apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata: {name: swebench-verified}
spec:
  target: {agent: {cli: {command: [my-agent]}}}
  dataset: {uri: "swebench://verified"}
  evaluators: [{ref: task-success}]
  gates:
    - {metric: task-success, min: 0.3}`}
    </pre>
  );
}

function Mcp() {
  return (
    <div className={`absolute inset-x-6 top-6 space-y-2 text-xs`}>
      <div className="ml-auto w-fit rounded-xl rounded-br-sm bg-indigo-500 px-3 py-2 text-white">Did my change break the evals?</div>
      <div className="w-fit rounded-xl rounded-bl-sm border border-neutral-200 bg-neutral-50 px-3 py-2 font-mono dark:border-white/10 dark:bg-white/[.03]">
        → evalsi.run <span className="text-neutral-400">capitals.yaml</span><br />
        exact-match 0.93 <span className="text-teal-500">▲ 0.04</span> · gate <span className="text-teal-500">passed</span>
      </div>
    </div>
  );
}

function Identity() {
  const rows = [["alice@acme", "editor", "support"], ["ci-bot", "runner", "support"], ["gateway", "ingest", "support"]];
  return (
    <div className={`absolute inset-x-6 top-6 overflow-hidden rounded-lg border border-neutral-200 font-mono text-[11.5px] dark:border-white/10`}>
      {rows.map(([who, role, proj]) => (
        <div key={who} className="grid grid-cols-3 border-b border-neutral-200 px-3 py-2 last:border-0 dark:border-white/10">
          <span>{who}</span><span className="text-indigo-500 dark:text-indigo-300">{role}</span><span className="text-neutral-400">{proj}</span>
        </div>
      ))}
    </div>
  );
}

function Scale() {
  const items = ["PostgreSQL", "ClickHouse", "S3", "NATS", "KEDA", "HA replicas", "air-gapped"];
  return (
    <div className={`absolute inset-x-6 top-6 flex flex-wrap gap-2`}>
      {items.map((i) => (
        <span key={i} className="rounded-full border border-neutral-200 bg-neutral-50 px-3 py-1 text-xs dark:border-white/10 dark:bg-white/[.03]">{i}</span>
      ))}
    </div>
  );
}

export default function Features({ base }: { base: string }) {
  const features = [
    { Icon: ShieldCheck, name: "Sandboxed, fail-closed", description: "Firecracker where available, then bubblewrap or Landlock, then hardened pods. If nothing qualifies, nothing runs.", href: `${base}/docs/decisions/0005-sandbox-isolation-ladder/`, cta: "How the ladder works", className: "md:col-span-2", background: <SandboxLadder /> },
    { Icon: Puzzle, name: "Pluggable by design", description: "Existing frameworks and benchmarks plug in as isolated adapters.", href: `${base}/docs/guides/plugins/`, cta: "Plugins and adapters", className: "md:col-span-1", background: <Adapters /> },
    { Icon: Sparkles, name: "Inline guardrails", description: "Redact, evaluate and block in the request path through agentgateway.", href: `${base}/integrations/agentgateway/`, cta: "Guard your traffic", className: "md:col-span-1", background: <Guardrail /> },
    { Icon: Boxes, name: "Kubernetes native", description: "Helm charts and an operator with EvalRun, OnlineEvalPolicy, Evaluator and SandboxClass resources.", href: `${base}/integrations/kubernetes/`, cta: "Deploy on Kubernetes", className: "md:col-span-2", background: <Crd /> },
    { Icon: Cpu, name: "MCP for coding agents", description: "Agents check their own changes with evalsi mcp.", href: `${base}/docs/guides/mcp/`, cta: "Connect over MCP", className: "md:col-span-1", background: <Mcp /> },
    { Icon: KeyRound, name: "Identity and access", description: "OIDC, API keys, project RBAC, CEL rules and an audit log.", href: `${base}/docs/guides/identity/`, cta: "Set up access", className: "md:col-span-1", background: <Identity /> },
    { Icon: ServerCog, name: "Runs in your environment", description: "Self-hosted with your models, storage, identity and secrets.", href: `${base}/docs/guides/kubernetes/`, cta: "Scale it out", className: "md:col-span-1", background: <Scale /> },
  ];
  return (
    <BentoGrid>
      {features.map((f) => <BentoCard key={f.name} {...f} />)}
    </BentoGrid>
  );
}
