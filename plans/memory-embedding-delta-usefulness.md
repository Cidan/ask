# Plan — embedding-delta usefulness signal for memmy

Status: deferred. Documented for future work after the first hook slice
ships. Do not build until we have a real embedder (Gemini or local) and
a real corpus to measure against.

## Problem

When a `Recall` surfaces nodes for a turn, every surfaced node receives
an implicit `+NodeDelta` reinforcement bump (memmy DESIGN.md §8.2). That
is the always-on Hebbian credit: surfacing IS reinforcement.

But surfacing-by-vector-similarity is not the same as the agent
*actually using* the memory. A node may be vector-similar to the prompt
without contributing to the final response. Its implicit bump is then
unearned — it pulls the corpus toward whatever happens to be vector-near
each new prompt, instead of toward what historically *helped*.

We need an "earned its keep" signal that distinguishes:

- Useful: the response engaged with the recalled content beyond what
  the prompt alone would have produced.
- Not useful: the response stayed at the prompt's similarity baseline,
  or moved away from the recalled content.

Substring / keyword matching on the assistant text is not viable —
it falls apart under paraphrase, acronym substitution, and
prose-vs-code vocabulary mismatch. The semantically-correct signal is
already available in memmy itself, in vector form.

## Approach

Re-embed the assistant's final message at turn end. For every node
that was injected this turn, compute:

```
δ_node = cos(assistant_text, node) − cos(prompt, node)
```

Three regions:

- `δ > +ε` — the response gravitated toward the node beyond what the
  prompt would predict. The node likely shaped the response. **Action:
  no demote.** (Implicit Recall bump stands; it earned its keep.)
- `δ < −ε` — the response actively moved away from the node. The
  recall mis-fired or was contradicted. **Action: explicit `Demote`**
  to cancel the implicit Recall bump. Net weight change for the turn
  is zero (or near zero), and the node will not pull ahead the way
  genuinely useful nodes do.
- `|δ| ≤ ε` — ambiguous. **Action: leave alone.** memmy's natural
  decay handles slow erosion of marginal hits.

`Reinforce` is intentionally **not** called in this path. Within the
default 60-second refractory window after Recall, an explicit
`Reinforce` is dropped silently anyway (DESIGN.md §15) — the implicit
bump already credited the node. Calling Reinforce here would be a
no-op masquerading as code.

`Demote` is not refractory-gated and not log-dampened (DESIGN.md §8.2),
so a per-turn negative bump always applies at full magnitude.

## Why this isn't fragile

- Paraphrase-robust by construction. Cosine similarity in embedding
  space captures semantic alignment regardless of word order, acronym
  use, or code-vs-prose surface form.
- Calibrated against the prompt baseline, not against an absolute
  similarity threshold. A prompt about "session ID resolution" and a
  node about "session ID resolution" will already be highly cosine-
  similar — the question is whether the response moves the needle
  *further*.
- The threshold `ε` is a single tunable. We can A/B against the
  no-automatic-demote baseline (option B in the conversation that
  produced this plan) and measure recall quality over time.

## Why we are not building this yet

1. **No real embedder.** Until ask is using Gemini (or a local
   embedder), the embedding space is the deterministic-but-meaningless
   `fake` embedder, which would produce arbitrary δ values.
   Implementing the signal against fake embeddings would let us
   exercise the *plumbing*, but the *behavior* would be noise.
2. **No corpus.** With an empty (or first-week) corpus, decay and
   reinforcement dynamics have not stabilized. Adding an automated
   demote on top of a fragile baseline would compound errors.
3. **Option B is provably non-destructive.** Recall + time decay is
   already a usefulness gradient; useful memory accrues bumps via
   re-recall, useless memory withers. We get the slow-but-correct
   behavior for free. (B) is the right baseline to measure (A)
   against once we have data.

## Implementation sketch (when revisited)

In the Stop hook handler, after the Write of the turn summary:

```go
// Re-embed the assistant text once.
respVec, err := embedder.Embed(ctx, embed.EmbedTaskRetrievalQuery, []string{assistantText})
if err != nil { return /* fall through; no demote this turn */ }

// promptVec is the vector we already used for Recall this turn — cache it.
for _, hit := range injectedThisTurn {
    nodeVec, err := storage.GetNodeVector(hit.NodeID)
    if err != nil { continue }
    deltaSim := cosine(respVec, nodeVec) - cosine(promptVec, nodeVec)
    switch {
    case deltaSim > epsilon:
        // earned its keep — no action
    case deltaSim < -epsilon:
        _, _ = svc.Demote(ctx, memmy.DemoteRequest{Tenant: tenant, NodeID: hit.NodeID})
    }
}
```

Open questions for that future PR:

- Is `epsilon` static, or adapted per tenant from observed δ
  distributions?
- Do we cap how many demotes can fire per turn, to limit blast radius
  of a misaligned response?
- Should we batch-demote via `Mark` (with negative strength) instead of
  per-node `Demote` calls, when many nodes are being demoted at once?
- Is the assistant's final text the right target, or should we also
  consider the assistant's tool calls (which encode the agent's actual
  workflow choices)?

## Non-goals

- This plan does not change Write semantics (Stop hook still writes
  per-file + per-turn observations as in option B).
- This plan does not change Recall semantics. The implicit reinforcement
  bump remains as-is — it is the floor we are subtracting on demote, not
  something we replace.
- This plan does not introduce a "useful" detector based on substring
  matching, LLM judges, or any heuristic outside the embedding space we
  already maintain.
