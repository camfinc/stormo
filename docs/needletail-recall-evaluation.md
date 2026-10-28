# Deterministic recall replay

Run `go test ./internal/needletail/memory ./internal/needletail/loop -run TestRecall -v`.
The loop tests require permission to bind a local HTTP test server.

The comparison fixture replays three identical relevant turns (first question, follow-up,
compacted descendant) against fresh identical synthetic stores. It explicitly emulates the
former conversation-wide suppression baseline; the production implementation has no cooldown.
No model is called: model version is `deterministic-none`, request model is the loop fixture's
stub configuration, and recall prompt version is `recall-header-v1` (`RecallHeader`).

| Policy | Relevant recall | Injected UTF-16 units, including fence |
|---|---:|---:|
| Memory disabled | 0/3 | 0 |
| Former conversation suppression | 1/3 | 315 |
| Per-turn candidate | 3/3 | 945 |

These are exact fixture counts, not estimates of live task correctness or token usage. More
recall increases injected characters on repeated turns. Tokens, live task success, latency,
reflection cost, repeated-error rate, and stochastic uncertainty are unmeasured. Numerical
quality/cost rollout thresholds still require agreement before retrieval tuning.

Separate held-out lexical fixtures are measurement sentinels and have not been used to tune
ranking thresholds. English and Spanish same-language examples retrieve their relevant record.
A translation paraphrase misses. An irrelevant question containing `invoice` retrieves an
invoice record: this is an explicitly asserted known false positive, not a quality success.
One example per category cannot establish language or audience population performance.

Regression gates cover current text after edit, repeated turns, restart, compaction, expiry,
deletion, audience changes, trivial gating, and packing a smaller candidate after an oversized
candidate. Actual request tests check recall after repeat/edit/compaction/cancellation retry,
removal after deletion, stable per-turn injection across tool requests, and absence of recalled
text from both system prompts and persisted transcripts. Existing scope and fence tests provide
additional audience and adversarial-text gates. Outcome evidence and rejection/restore gates
are covered by their owning suites; this replay does not claim to evaluate those subsystems.

The fixtures contain only synthetic Acme data. No production transcripts, embeddings, external
model calls, or private archives enter the evaluation.
