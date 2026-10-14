# Evals.si website

Astro + Starlight site: a landing page (`src/pages/index.astro`), docs and examples.

The docs and examples are **generated from the repository** (`../docs`, `../examples`, `../README.md`) by `scripts/sync-content.mjs`, so edit those files, not `src/content/`.

```bash
npm install
npm run dev       # sync + dev server at http://localhost:4321/
npm run build     # sync + static build into dist/
node scripts/check-links.mjs   # after a build
```

Deployed to GitHub Pages by `.github/workflows/website.yml` (enable Pages with source "GitHub Actions" in the repo settings). The site is served from the custom domain `https://evalsi.dev` (set in the repo's Pages settings), so the base path is `/`. To serve it from the project URL instead, build with `SITE_BASE=/Evals.si SITE_URL=https://abhishek-rnjn.github.io`.

## Search and link previews

- Every page has a canonical URL, Open Graph and Twitter tags and a sitemap (`sitemap-index.xml`). An imported doc's description is its first paragraph, cut near 160 characters (`describe()` in `scripts/sync-content.mjs`).
- The link-preview card is `public/og.png`, rendered by `node scripts/og-image.mjs` (needs the Inter font). Run it again after changing the card's text.
- The landing page carries JSON-LD (`SoftwareSourceCode`) that ties the site to the GitHub repository and the `evalsi` and `evalsi-harness` PyPI packages.
- **Google Search Console:** `https://evalsi.dev/` is verified with the HTML-tag method. Its `content` value is the default in `astro.config.mjs` and `src/pages/index.astro` (it is public, in every page); a `GOOGLE_SITE_VERIFICATION` repository variable overrides it. Keep the tag after verifying, or the property becomes unverified. The sitemap to submit is `sitemap-index.xml`.

## Landing page components

The landing page uses React islands with Tailwind CSS v4 (`src/styles/landing.css`, landing page only). The components in `src/components/ui/` are adapted from [Easy UI](https://github.com/DarkInventor/easy-ui) and Magic UI (MIT); see `THIRD_PARTY_NOTICES.md`.
