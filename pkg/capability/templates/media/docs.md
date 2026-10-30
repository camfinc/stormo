Give one video or audio file (`files: ["/shared/<layer>/…/clip.mp4"]`; or, from an upload you were
sent, attach it to the job). Pick the operation with `op`:

- `probe`: duration, size, resolution, fps, codecs, whether it has audio.
- `qa`: probe plus integrated loudness and `count` stills spread across it (default 3) and a
  contact sheet: look at the sheet before you deliver a render.
- `stills`: PNG frames at the seconds in `at`, or `count` of them spread across it, with a contact
  sheet.
- `loudness`: integrated loudness (LUFS), true peak (dBTP) and loudness range.
- `master`: a copy normalized to `lufs` (default -16) under `true_peak` (default -1.5) with a
  two-pass loudnorm; the video stream is copied as it is.
- `cover_candidates`: the video's strongest moments for a cover (scene cuts and samples, scored
  for sharpness, contrast and exposure; black, blown-out and look-alike frames left out) as a
  labelled contact sheet (`#n time`) and their times and scores. Look at the sheet and pick the
  moment that tells the story; scores only break ties.
- `cover`: the frame at `at` (or the best scored one) fitted to each of `formats` as a JPEG:
  `fit` crop or blur (the whole frame over a blurred copy of itself), `focus` to move a crop
  toward the subject. Clean frames: put words on them with an HTML page (the browser's page
  rendering) if you need them.

Answers carry what was measured and `files`, the names written to your output folder.
