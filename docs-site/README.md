# docs-site

The documentation site for `vinted-cli`, built with Fumadocs on Next.js and exported
to GitHub Pages at `https://vinted.micr.dev` by `.github/workflows/pages.yml`.

The build copies `scripts/install.sh` to the exported `/install.sh` URL. Keep the
installer in that one source file; the short URL and install examples use it directly.
Search runs in the browser against the generated `/api/search` index. `/llms.txt`
is also generated during the build, so the published site needs no running server.

```bash
npm ci
npm run dev  # http://localhost:3000/docs
npm run typecheck
npm run build
npm start    # serve the static export at http://localhost:3000/docs/
```

The static preview command requires Python 3.

`npm ci` runs `fumadocs-mdx` through `postinstall` to generate the content types before
typechecking or building. CI uses Node.js 22.

## Command reference

`content/docs/reference/*.mdx` is generated from the CLI's cobra tree by `cmd/docsgen`. Do not
edit those pages: change the command's `Short`, `Long` or flag usage in Go and regenerate.

```bash
make docs        # from the repository root
make docs-check  # what CI runs: regenerate and fail if anything moved
```

`content/docs/reference/index.mdx` is written by hand and is left alone by the generator.

## Theme

The accent is `#017681`, the fill of the vinted-cli logo, sampled from the source SVG. It
is used for links, the active nav item, focus rings and highlights in both modes. Dark mode
is the default and ignores the OS preference (`enableSystem: false`); the nav toggle still
switches to light and the choice is remembered. Dark mode is a true black `#000000` page with
white text and no decorative chrome. `app/icon.png`, `app/apple-icon.png`, `app/favicon.ico`
and `app/opengraph-image.png` are generated from the logo.
