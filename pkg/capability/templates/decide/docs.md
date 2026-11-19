Typed decisions about a state with a structured decision model (default `typesafe/jev-1.14` on
OpenRouter): no text, a probability per answer, in under a second and for a fraction of a cent.

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
- Decide by thresholds you tested on your own data, per question and per model version (pass
  `model` to pin another): act at a high probability, leave the middle to a person, and treat an
  error or a missing answer as no decision, never as a yes. The same state can move a probability
  by a few hundredths from one call to the next.
