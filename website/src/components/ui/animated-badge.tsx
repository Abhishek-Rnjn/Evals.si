// Ported from Easy UI (components/easyui/animated-badge.tsx), MIT. See THIRD_PARTY_NOTICES.md.
import { cn } from "../../lib/utils";

export function AnimatedBadge({ text, className }: { text: string; className?: string }) {
  return (
    <span className={cn("relative inline-block overflow-hidden rounded-full bg-indigo-950 p-[0.125em]", className)}>
      <span className="absolute inset-0 animate-[spin_4s_linear_infinite] bg-gradient-to-t from-transparent via-indigo-400 to-transparent" />
      <span className="relative block rounded-full bg-indigo-950 px-[0.75em] py-[0.0625em] text-xs font-medium text-indigo-200">{text}</span>
    </span>
  );
}
