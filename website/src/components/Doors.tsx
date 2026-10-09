import { Activity, Gauge, Play, Trophy } from "lucide-react";
import { MagicCard } from "./ui/magic-card";

export default function Doors({ base }: { base: string }) {
  const doors = [
    { Icon: Gauge, name: "Score", tag: "evalsi eval", text: "Grade outputs you already have. Every mean ships with a confidence interval; records an evaluator can't judge are skipped, never scored zero.", href: `${base}/docs/get-started/` },
    { Icon: Play, name: "Run", tag: "evalsi run", text: "Durable, resumable runs with trials, gates and budgets, including agents on sandboxed tasks and benchmarks.", href: `${base}/docs/guides/agent-runs/` },
    { Icon: Activity, name: "Watch", tag: "evalsi policy", text: "Continuously evaluate live OpenTelemetry traces with sampling, cascades, windowed alerts and promotion to datasets.", href: `${base}/examples/watch/` },
    { Icon: Trophy, name: "Reward", tag: "RewardService", text: "Evaluators as RL rewards for TRL, verl and OpenRLHF, and every checkpoint checked against the base model.", href: `${base}/docs/guides/fine-tuning/` },
  ];
  return (
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      {doors.map(({ Icon, ...d }) => (
        <MagicCard key={d.name} href={d.href}>
          <div className="flex h-full flex-col gap-3">
            <div className="flex items-center justify-between">
              <span className="flex size-10 items-center justify-center rounded-xl bg-gradient-to-br from-indigo-500/15 to-teal-400/15 ring-1 ring-indigo-500/20">
                <Icon className="size-5 text-indigo-500 dark:text-indigo-300" />
              </span>
              <code className="rounded-md bg-neutral-100 px-2 py-0.5 font-mono text-[11px] text-neutral-500 dark:bg-white/5 dark:text-neutral-400">{d.tag}</code>
            </div>
            <h3 className="text-lg font-semibold">{d.name}</h3>
            <p className="text-sm leading-relaxed text-neutral-500 dark:text-neutral-400">{d.text}</p>
          </div>
        </MagicCard>
      ))}
    </div>
  );
}
