---
description: Generate purpose summaries for indexed code symbols using this agent (no API key or local model needed)
---

Enrich the contextmaxxer index of this project with purpose summaries, acting
as the generator yourself.

Steps:

1. Run `contextmaxxer enrich -emit-pending .` — it writes
   `.contextmaxxer/pending-docs.json` with the symbols that need summaries
   (qualified_name, kind, file, signature, body excerpt).
2. Read `pending-docs.json` in batches. For every symbol write ONE English
   sentence (max 35 words) describing what it does and why it exists. Include
   concrete specifics of this exact symbol: key inputs, outputs, conditions or
   side effects. Never use filler like "handles requests" or "manages data",
   and never compare the symbol to other symbols — summaries that look alike
   make similar symbols indistinguishable in embedding space.
3. Merge your summaries into `.contextmaxxer/synthetic-docs.json` as a JSON
   object `{ "qualified_name": "summary", ... }`, preserving existing entries.
4. Run `contextmaxxer -force index .` so the summaries reach the embeddings.
5. Delete `.contextmaxxer/pending-docs.json` and report how many symbols you
   summarized.

$ARGUMENTS
