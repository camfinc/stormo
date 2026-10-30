Give the Remotion project's folder as the first file (`files: ["/shared/<layer>/…/my-video"]`),
plus any media it loads from outside it as further files. The project is copied without
`node_modules` and `.git`: it is bundled against the packages this worker provides (`remotion` and
`@remotion/` media-utils, transitions, shapes, paths, noise, google-fonts and three, plus `react`,
`react-dom`, `three` and `@react-three/fiber`), so imports of anything else fail. Put images, audio
and video under the project's `public/` and load them with `staticFile()`.

Answer: `{file, width, height, fps, duration_seconds, frames, codec, stills, contact, loudness}`;
the video, stills (`<name>-<seconds>s.png`) and `contact.png` are written to your output folder.

- `scale: 0.5` and a `frames` range give quick drafts; render the full piece once the draft looks
  right. Renders take time: start them with cap_start and let the notice wake you.
- `master: {lufs: -16, true_peak: -1.5}` normalizes the soundtrack (two-pass loudnorm with a
  true-peak limiter) and keeps the video stream as rendered.
- A render that fails answers the bundler's or Remotion's error: fix the project and start again.
