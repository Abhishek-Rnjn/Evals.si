// Ported from Easy UI (components/magicui/bento-grid.tsx), MIT. See THIRD_PARTY_NOTICES.md.
import type { ComponentType, ReactNode } from "react";
import { ArrowRight } from "lucide-react";
import { cn } from "../../lib/utils";

export function BentoGrid({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn("grid w-full auto-rows-[23rem] grid-cols-1 md:auto-rows-[20rem] gap-4 md:grid-cols-3", className)}>{children}</div>;
}

interface BentoCardProps {
  name: string;
  className?: string;
  background?: ReactNode;
  Icon: ComponentType<{ className?: string }>;
  description: string;
  href: string;
  cta: string;
}

export function BentoCard({ name, className, background, Icon, description, href, cta }: BentoCardProps) {
  return (
    <a
      href={href}
      className={cn(
        "group relative col-span-1 flex flex-col justify-between overflow-hidden rounded-2xl",
        "bg-white [box-shadow:0_0_0_1px_rgba(0,0,0,.03),0_2px_4px_rgba(0,0,0,.05),0_12px_24px_rgba(0,0,0,.05)]",
        "transform-gpu dark:bg-[#0b0d17] dark:[border:1px_solid_rgba(255,255,255,.1)] dark:[box-shadow:0_-20px_80px_-20px_#ffffff1f_inset]",
        className,
      )}
    >
      <div className="absolute inset-0 overflow-hidden [mask-image:linear-gradient(to_bottom,#000_40%,transparent_68%)]">{background}</div>
      <div className="pointer-events-none z-10 mt-auto flex transform-gpu flex-col gap-1 p-6 transition-all duration-300 group-hover:-translate-y-10">
        <Icon className="h-10 w-10 origin-left transform-gpu text-indigo-500 transition-all duration-300 ease-in-out group-hover:scale-75 dark:text-indigo-300" />
        <h3 className="text-xl font-semibold text-neutral-800 dark:text-neutral-100">{name}</h3>
        <p className="max-w-lg text-[15px] leading-relaxed text-neutral-500 dark:text-neutral-400">{description}</p>
      </div>
      <div className="pointer-events-none absolute bottom-0 flex w-full translate-y-10 transform-gpu flex-row items-center p-4 px-6 opacity-0 transition-all duration-300 group-hover:translate-y-0 group-hover:opacity-100">
        <span className="inline-flex items-center text-sm font-medium text-indigo-600 dark:text-indigo-300">
          {cta}
          <ArrowRight className="ml-2 h-4 w-4" />
        </span>
      </div>
      <div className="pointer-events-none absolute inset-0 transform-gpu transition-all duration-300 group-hover:bg-black/[.03] dark:group-hover:bg-white/[.03]" />
    </a>
  );
}
