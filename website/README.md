# IvaraBase website

The documentation site for IvaraBase, built with [Astro Starlight](https://starlight.astro.build). `npm run build` produces a plain static site in `dist/` that any static host can serve.

## Build

```sh
cd website
npm ci
npm run build        # static site in website/dist/
npm run preview      # serve dist/ at http://localhost:4321
npm run dev          # live-reloading dev server
```

Node 20 or newer. Dependency versions are pinned exactly in `package.json`.

## Where the content comes from

The pages are **not** edited here. `../docs/*.md` is the single source of truth, and `scripts/sync-docs.mjs` copies it into the site before every `dev` and `build` (or run it alone with `npm run sync`):

- `../docs/FOO.md` becomes `src/content/docs/docs/foo.md`, served at `/docs/foo/`. The first `# H1` becomes the page title (and is removed from the body), the first paragraph becomes the description, and "Edit page" points at `docs/FOO.md` on GitHub `main`.
- Links are rewritten: `FOO.md` and `FOO.md#anchor` go to the page, `../README.md`, `../CHANGELOG.md` and other repository files go to GitHub, and `images/x.png` goes to the synced copy.
- `../docs/images/*` is copied to `public/docs-images/`. A `dashboard-dark.png` / `dashboard-light.png` (or `dashboard.png`) there is shown on the landing page; without one, the landing page just leaves it out.

Both generated folders are gitignored. Which docs appear, their order and sidebar labels are set in `docs.manifest.mjs`. A listed file that doesn't exist yet is skipped with a warning, and is left out of the sidebar, so the build still passes.

The landing page is `src/content/docs/index.mdx`; the colours and fonts are in `src/styles/theme.css`.

## Domain and base path

The final domain isn't decided yet, so the site URL and base path come from environment variables at build time:

| Variable | Default | For |
|---|---|---|
| `SITE` | `https://base.ivara.io` | the full origin, used for canonical URLs and the sitemap |
| `BASE` | `/` | the path the site is served under, e.g. `/ivarabase/` for `https://user.github.io/ivarabase/` |

```sh
SITE=https://docs.example.com npm run build
SITE=https://example.github.io BASE=/ivarabase/ npm run build
```

## Deploy

`dist/` is self-contained static HTML, CSS and JS (search runs in the browser with Pagefind). There are no analytics or tracking scripts.

- **Any static host:** upload the contents of `website/dist/`.
- **Vercel** (used for base.ivara.io): import the repo, set **Root Directory** to `website` and keep "Include files outside the root directory" on (the build reads `../docs`). `vercel.json` sets the build and output. Then add the domain under Settings → Domains.
- **Cloudflare Pages:** root directory `website`, build command `npm ci && npm run build`, output directory `dist`. Set `SITE` (and `BASE` if needed) as environment variables.
- **Netlify:** base directory `website`, build command `npm run build`, publish directory `website/dist`.
- **Vercel:** root directory `website`, framework preset Astro (or "Other" with build command `npm run build` and output directory `dist`).
- **nginx:**

  ```nginx
  server {
      server_name docs.example.com;
      root /var/www/ivarabase-docs;      # the contents of website/dist/
      location / {
          try_files $uri $uri/ $uri.html =404;
      }
      error_page 404 /404.html;
  }
  ```

The build needs the repository's `docs/` folder next to `website/`, so build from a full checkout, not from `website/` alone.
