Typed decisions about a state, pictures included, with a structured decision model (default
`typesafe/jev-1.14` on OpenRouter): no text, a probability per answer, in under a second and for a
fraction of a cent.

- `questions` by name. `noul` (yes/no) answers `{"type":"noul","noul":0.96}`, the probability of
  yes. `choice` answers `{"choice","confidence","probabilities":{option:p}}`. `score` answers
  `{"score","confidence","probabilities":{"0":p,…},"legend"}`, levels counted from 0. An answer of
  type `refus` carries no value: treat it as unknown.
- `state`: one decision (`answers`), or `states` `{id: state}`: a batch asked the same questions
  in parallel (`results` `{id: {"answers"} | {"error"}}`, with `decided` and `failed`; the job
  fails only when none was decided). `cost_usd` is what it cost.
- Ask narrow questions, one proposition each ("is the client upset?", not "is this chat a
  problem?"), and say in `criteria` what makes it true and what does not. The questions are asked
  independently: one never sees another's answer. A question costs a few tokens; the state is billed
  once however many questions it gets.
- Put what to judge apart from its context in the state (`{"earlier": [...], "new": [...]}`) and
  say which part to judge in the instructions.
- Keep a state under ~32 KB: past that the model switches to a slower, less accurate mode.
- **Pictures**: hand them over (`files`, PNG or JPEG) and name each one as `{"image": "<file name>"}`
  where the model should look at it: in the state, in a question's `instructions` (an array, e.g.
  `["Is this the same person as in", {"image": "frame.png"}, "?"]`) or as a choice option's value.
  They are fitted into the API's bounds (each side 256-1568 px), count as input tokens, and every
  state with one runs in the slower long-context mode. EXIF rotation is not applied: send pictures
  upright. A `box` question (`{"type": "box", "instructions": "the face of the woman in \`photo\`"}`,
  no criteria) answers `{"box": {"top", "bottom", "left", "right"}, "confidence"}` as fractions of
  the picture; it always answers a box, so ask a `noul` alongside to know whether the thing is there.
- Decide by thresholds you tested on your own data, per question and per model version (pass
  `model` to pin another): act at a high probability, leave the middle to a person, and treat an
  error or a missing answer as no decision, never as a yes. The same state can move a probability
  by a few hundredths from one call to the next.
