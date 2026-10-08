// Ported from Easy UI (components/magicui/magic-card.tsx), MIT. See THIRD_PARTY_NOTICES.md.
// Simplified: each card tracks the pointer itself instead of a shared container.
import { useRef, type CSSProperties, type ReactNode, type PointerEvent } from "react";
import { cn } from "../../lib/utils";

interface MagicCardProps {
  className?: string;
  children?: ReactNode;
  size?: number;
  href?: string;
}

export function MagicCard({ className, children, size = 360, href }: MagicCardProps) {
  const ref = useRef<HTMLAnchorElement & HTMLDivElement>(null);
  const move = (e: PointerEvent) => {
    const el = ref.current;
    if (!el) return;
    const r = el.getBoundingClientRect();
    el.style.setProperty("--mouse-x", `${e.clientX - r.left}px`);
    el.style.setProperty("--mouse-y", `${e.clientY - r.top}px`);
  };
  const Tag = (href ? "a" : "div") as "a";
  return (
    <Tag
      ref={ref}
      href={href}
      onPointerMove={move}
      style={{ "--mask-size": `${size}px` } as CSSProperties}
      className={cn(
        "group relative z-0 block h-full w-full overflow-hidden rounded-2xl p-6 [--mouse-x:-999px] [--mouse-y:-999px]",
        "bg-neutral-200 dark:bg-white/10",
        "bg-[radial-gradient(var(--mask-size)_circle_at_var(--mouse-x)_var(--mouse-y),#818cf8,transparent_100%)]",
        className,
      )}
    >
      <div className="absolute inset-px -z-10 rounded-[15px] bg-white dark:bg-[#0b0d17]" />
      <div className="pointer-events-none absolute inset-px -z-10 rounded-[15px] opacity-0 transition-opacity duration-300 group-hover:opacity-100 bg-[radial-gradient(calc(var(--mask-size)*0.8)_circle_at_var(--mouse-x)_var(--mouse-y),rgba(129,140,248,0.10),transparent_70%)]" />
      {children}
    </Tag>
  );
}
