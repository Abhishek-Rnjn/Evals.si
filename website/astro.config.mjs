import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import react from "@astrojs/react";
import tailwindcss from "@tailwindcss/vite";

const base = (process.env.SITE_BASE ?? "/").replace(/\/$/, "") || "/";
const site = process.env.SITE_URL ?? "https://evalsi.dev";
const repo = process.env.REPO_URL ?? "https://github.com/Abhishek-Rnjn/Evals.si";
// The link-preview card (scripts/og-image.mjs), absolute as Open Graph requires.
const ogImage = `${site}${base === "/" ? "" : base}/og.png`;
// Google Search Console's HTML-tag verification for
// https://evalsi.dev/ (the value is public: it is in
// every page). A GOOGLE_SITE_VERIFICATION repository variable overrides it;
// the same default is in src/pages/index.astro.
const googleVerification = process.env.GOOGLE_SITE_VERIFICATION || "71Ou0Qy18e2sRbPlZ9OXapiu2-xZAJnnmPcVzmDdetM";

const guides = [
  "onboarding", "dataset-snapshots", "judge-calibration", "failure-inbox", "dataset-catalog", "release-decisions", "release-workbench", "agent-runs", "built-in-harness", "enterprise-deployment", "gate-an-agent-in-ci", "support-refund-suite", "evaluate-rag", "annotation", "fine-tuning", "guardrails", "identity",
  "kubernetes", "agentgateway-kubernetes", "mcp", "plugins", "web-ui", "webhooks", "trace-sources", "integrate-an-agent-harness",
  "agent-frameworks", "operations", "end-to-end", "deepeval", "harness2", "onboarding-usability",
];
const examples = [
  "first-time-onboarding", "quickstart", "runs", "agent-gate", "rag", "agents", "watch", "sources", "guardrails", "annotation",
  "finetuning", "custom-evaluator", "wasm", "auth", "server", "ci",
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
        { tag: "meta", attrs: { property: "og:image", content: ogImage } },
        { tag: "meta", attrs: { property: "og:image:width", content: "1200" } },
        { tag: "meta", attrs: { property: "og:image:height", content: "630" } },
        { tag: "meta", attrs: { property: "og:image:alt", content: "Evals.si: evaluate AI before you ship, while it runs, and as you train it" } },
        { tag: "meta", attrs: { name: "twitter:image", content: ogImage } },
        { tag: "meta", attrs: { name: "keywords", content: "evalsi, Evals.si, agent evaluation, AI agent testing, agent CI gates, RAG evaluation, RAG faithfulness, RL rewards, fine-tuning evaluation, LLM evaluation, LLM-as-a-judge, evals, RL rewards, OpenTelemetry, MLflow, Kubernetes" } },
        ...(googleVerification ? [{ tag: "meta", attrs: { name: "google-site-verification", content: googleVerification } }] : []),
        { tag: "link", attrs: { rel: "preconnect", href: "https://fonts.googleapis.com" } },
        { tag: "link", attrs: { rel: "stylesheet", href: "https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" } },
      ],
      favicon: "/favicon.svg",
      description: "Evals.si (pip install evalsi): evaluate AI before you ship, while it runs and as you train it. Agents, RAG pipelines, LLMs and models, with CI gates, release decisions, live trace scoring, a built-in agent harness and RL rewards. Self-hosted and open source.",
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
          { label: "First-time onboarding course", slug: "docs/tutorials" },
          { label: "Built-in agent harness", slug: "docs/guides/built-in-harness" },
          { label: "Release decisions", slug: "docs/guides/release-decisions" },
        ] },
        { label: "Onboarding tutorials", items: [
          "01-install", "02-score", "03-tool-loop", "04-failures", "05-state-and-handoff", "onboarding-study",
        ].map((name) => ({ slug: `docs/tutorials/${name}` })) },
        { label: "Sandbox and provider tutorials", items: [
          "00-onboarding-study", "01-install-and-first-eval", "02-tinylm-fake-model", "03-built-in-harness",
          "04-trials-and-flakiness", "05-record-replay-and-compare", "06-faults-injection-and-approvals",
          "07-mocked-tools", "08-server-ui-and-regressions", "09-ci", "10-real-haiku", "11-local-kind",
          "12-macos-reverification", "13-aks-onboarding",
        ].map((name) => ({ slug: `docs/tutorials/${name}` })) },
        { label: "Releases", items: [
          { label: "Changelog", slug: "docs/changelog" },
          { label: "GitHub releases", link: "https://github.com/Abhishek-Rnjn/Evals.si/releases" },
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
          { label: "Product roadmap", slug: "docs/product-roadmap" },
          { label: "Product strategy", slug: "docs/product-strategy" },
          { slug: "docs/roadmap" },
          { label: "Decision records", slug: "docs/decisions", collapsed: true },
        ] },
      ],
    }),
  ],
});
