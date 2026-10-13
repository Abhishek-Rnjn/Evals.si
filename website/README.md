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

## Search and link previews

- Every page has a canonical URL, Open Graph and Twitter tags and a sitemap (`sitemap-index.xml`). An imported doc's description is its first paragraph, cut near 160 characters (`describe()` in `scripts/sync-content.mjs`).
- The link-preview card is `public/og.png`, rendered by `node scripts/og-image.mjs` (needs the Inter font). Run it again after changing the card's text.
- The landing page carries JSON-LD (`SoftwareSourceCode`) that ties the site to the GitHub repository and the `evalsi` and `evalsi-harness` PyPI packages.
- **Google Search Console:** add `https://abhishek-rnjn.github.io/Evals.si/` as a URL-prefix property, choose the HTML-tag method, and put the tag's `content` value in a repository variable named `GOOGLE_SITE_VERIFICATION` (Settings > Secrets and variables > Actions > Variables). The next deploy adds the tag to every page. Then submit `sitemap-index.xml` in Search Console.

## Landing page components

The landing page uses React islands with Tailwind CSS v4 (`src/styles/landing.css`, landing page only). The components in `src/components/ui/` are adapted from [Easy UI](https://github.com/DarkInventor/easy-ui) and Magic UI (MIT); see `THIRD_PARTY_NOTICES.md`.
