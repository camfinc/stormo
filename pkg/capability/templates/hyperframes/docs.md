Hand over the HyperFrames project's folder as the first file (`files: ["/shared/<layer>/…/my-reel"]`):
its `index.html` (or `composition`), the scripts it loads (GSAP as a local file), fonts and media,
all by relative path. `node_modules` and `.git` are not copied.

- `lint` first (`--json` findings: errors and warnings), then a `render` at `quality: draft`;
  look at it (the media capability's `qa` gives stills and loudness), then render
  `quality: delivery` once it holds.
- `validate`, `inspect` and `compositions` answer what the CLI reports about the project.
- Renders run in software (no GPU) with one Chrome per worker; a long or heavy piece takes
  minutes: start it with cap_start and let the notice wake you.
- `variables` fill the composition's `data-composition-variables` (titles, prices…): one project,
  many renders.

Answer: `{file, format, quality, size_mb, seconds, log}` for a render, the CLI's JSON (`result`)
for the others; the video is written to your output folder.
