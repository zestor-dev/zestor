# Zestor documentation site

Built with [Hugo](https://gohugo.io/) and the [Docsy](https://www.docsy.dev/) theme.

## Local build

```bash
npm ci
npm run build   # output in ./public/
```

`./public/`, `./resources/_gen/`, and `./node_modules/` are listed in the repo root `.gitignore`; do not commit them.

`npm run build` runs the pinned **extended** Hugo from `hugo-bin` (see `package.json`). Use this instead of a system `hugo` so the version matches production.
