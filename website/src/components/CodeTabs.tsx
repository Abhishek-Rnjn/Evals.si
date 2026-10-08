import { useState } from "react";
import { Check, Copy } from "lucide-react";
import { BorderBeam } from "./ui/border-beam";
import { cn } from "../lib/utils";

export interface CodeTab { label: string; lang: string; code: string }

export default function CodeTabs({ tabs }: { tabs: CodeTab[] }) {
  const [active, setActive] = useState(0);
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(tabs[active].code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {}
  };
  return (
    <div className="relative overflow-hidden rounded-2xl border border-neutral-200 bg-white text-left shadow-2xl shadow-indigo-500/10">
      <div className="flex items-center justify-between border-b border-neutral-200 bg-neutral-50/80 px-3">
        <div role="tablist" className="flex">
          {tabs.map((t, i) => (
            <button
              key={t.label}
              role="tab"
              aria-selected={i === active}
              onClick={() => setActive(i)}
              className={cn(
                "relative px-3 py-3 text-xs font-medium transition-colors sm:text-sm",
                i === active ? "text-neutral-900" : "text-neutral-500 hover:text-neutral-800",
              )}
            >
              {t.label}
              {i === active && <span className="absolute inset-x-3 -bottom-px h-px bg-gradient-to-r from-indigo-400 to-teal-300" />}
            </button>
          ))}
        </div>
        <button onClick={copy} aria-label="Copy code" className="rounded-md p-2 text-neutral-500 transition-colors hover:bg-neutral-100 hover:text-neutral-900">
          {copied ? <Check className="size-4 text-teal-600" /> : <Copy className="size-4" />}
        </button>
      </div>
      <pre className="overflow-x-auto p-5 font-mono text-[13px] leading-relaxed text-neutral-700"><code>{tabs[active].code}</code></pre>
      <BorderBeam size={220} duration={12} />
    </div>
  );
}
