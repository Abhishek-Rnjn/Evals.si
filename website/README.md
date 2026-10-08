# Evals.si website

Astro + Starlight site: a landing page (`src/pages/index.astro`), docs and examples.

The docs and examples are **generated from the repository** (`../docs`, `../examples`, `../README.md`) by `scripts/sync-content.mjs`, so edit those files, not `src/content/`.

```bash
npm install
npm run dev       # sync + dev server at http://localhost:4321/Evals.si/
npm run build     # sync + static build into dist/
node scripts/check-links.mjs   # after a build
```

Deployed to GitHub Pages by `.github/workflows/website.yml` (enable Pages with source "GitHub Actions" in the repo settings). For a custom domain such as `evals.si`, build with `SITE_BASE=/ SITE_URL=https://evals.si` and add `public/CNAME`.

## Landing page components

The landing page uses React islands with Tailwind CSS v4 (`src/styles/landing.css`, landing page only). The components in `src/components/ui/` are adapted from [Easy UI](https://github.com/DarkInventor/easy-ui) and Magic UI (MIT); see `THIRD_PARTY_NOTICES.md`.
