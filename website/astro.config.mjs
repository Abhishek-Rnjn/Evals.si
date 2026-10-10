import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import react from "@astrojs/react";
import tailwindcss from "@tailwindcss/vite";

const base = (process.env.SITE_BASE ?? "/Evals.si").replace(/\/$/, "") || "/";
const site = process.env.SITE_URL ?? "https://abhishek-rnjn.github.io";
const repo = process.env.REPO_URL ?? "https://github.com/Abhishek-Rnjn/Evals.si";

const guides = [
  "agent-runs", "annotation", "fine-tuning", "guardrails", "identity",
  "kubernetes", "agentgateway-kubernetes", "mcp", "plugins", "web-ui", "webhooks", "integrate-an-agent-studio",
  "agent-frameworks",
];
const examples = [
  "quickstart", "runs", "agents", "watch", "guardrails", "annotation",
  "finetuning", "wasm", "auth", "server", "ci",
];

export default defineConfig({
  site,
  base,
  vite: { plugins: [tailwindcss()] },
  integrations: [
    react(),
    starlight({
      title: "Evals.si",
      logo: { src: "./src/assets/logo.svg", alt: "Evals.si", replacesTitle: true },
      head: [
        { tag: "link", attrs: { rel: "preconnect", href: "https://fonts.googleapis.com" } },
        { tag: "link", attrs: { rel: "stylesheet", href: "https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" } },
      ],
      favicon: "/favicon.svg",
      description: "One entrypoint for evaluating ML models, LLMs, RAG systems and agents.",
      social: [{ icon: "github", label: "GitHub", href: repo }],
      customCss: ["./src/styles/theme.css"],
      components: {
        Footer: "./src/components/Footer.astro",
        // Light mode only: no theme switcher, and the theme is pinned to light.
        ThemeProvider: "./src/components/LightTheme.astro",
        ThemeSelect: "./src/components/NoThemeSelect.astro",
      },
      sidebar: [
        { label: "Start here", items: [
          { label: "Overview", slug: "docs" },
          { label: "Get started", slug: "docs/get-started" },
        ] },
        { label: "Integrations", items: [
          { label: "Overview", slug: "integrations" },
          { label: "agentgateway", slug: "integrations/agentgateway" },
          { label: "Kubernetes", slug: "integrations/kubernetes" },
          { label: "Agent platforms and studios", slug: "integrations/agent-platforms" },
        ] },
        { label: "Guides", items: guides.map((g) => ({ slug: `docs/guides/${g}` })) },
        { label: "Examples", items: [
          { label: "All examples", slug: "examples" },
          ...examples.map((e) => ({ slug: `examples/${e}` })),
        ] },
        { label: "Design", items: [
          { slug: "docs/architecture" },
          { slug: "docs/roadmap" },
          { label: "Decision records", slug: "docs/decisions", collapsed: true },
        ] },
      ],
    }),
  ],
});
