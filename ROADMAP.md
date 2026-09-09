# Roadmap

This roadmap describes direction, not release commitments. Priorities may move
when public beta evidence contradicts current assumptions.

## After the first release

v0.1.0 ships Windows amd64 and Linux amd64 archives with checksums, built by CI
on both platforms and gated on the packaged binary answering a real MCP
handshake. What that leaves:

- Validate installation with users who did not see the development environment.
- Collect reproducible retrieval misses and setup failures through issue forms.

## Reliability and distribution

- Add software-bill-of-materials and artifact provenance to releases.
- Evaluate signing once the release channel stabilizes.
- Add macOS only after the native tokenizer and ONNX Runtime path is verified on
  hosted and physical machines.
- Make first-run downloads and index progress easier to diagnose.

## Retrieval quality

- Expand the public regression suite with independently contributed cases.
- Publish the paired-agent benchmark harness so agent-level tables can be
  reproduced without reconstructing the protocol.
- Treat negative results as first-class research output and keep them separate
  from the supported product path.

## Research artifacts

Fine-tuned weights will be considered for publication only with training-data
provenance, an evaluation contract, a model card and a clearly compatible
license. Until then they remain recorded research, not a release feature.
