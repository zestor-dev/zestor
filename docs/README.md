# Zestor documentation site

Built with [Hugo](https://gohugo.io/) and the [Docsy](https://www.docsy.dev/) theme.

## Local build

```bash
npm ci
npm run build   # output in ./public/
```

`./public/`, `./resources/_gen/`, and `./node_modules/` are listed in the repo root `.gitignore`; do not commit them.

`npm run build` runs the pinned **extended** Hugo from `hugo-bin` (see `package.json`). Use this instead of a system `hugo` so the version matches production.

## Cloudflare Pages

Docsy’s `i18n/en.yaml` uses YAML scalars like `Yes` / `No`. Hugo **before ~0.152** parses those as booleans and fails with `unsupported file format bool`. This repo pins Hugo **0.152.2+** via `hugo-bin`.

1. **Root directory**: `docs` (or your equivalent Pages root).
2. **Build command**: `npm run build`  
   Do **not** call the platform `hugo` binary directly unless you set **`HUGO_VERSION`** to **0.152.2** or newer there as well.
3. **Build output directory**: `public`

Optional: set environment variable `HUGO_VERSION=0.152.2` if you switch back to a global `hugo` install and drop `hugo-bin`.
