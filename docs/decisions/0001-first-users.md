# 0001. First users: agent builders, agent platform builders, LLM app developers

- **Status:** Accepted, 2026-10-05 (design plan D1)

## Decision

The first users are agent builders, agent platform builders and LLM app developers. Classic ML evaluation and RL/fine-tuning evaluation stay in the design but are not what the early phases optimize for.

## Why

Online and offline agent evaluation is the least served of these needs and the one where a single entrypoint helps most. Agent platform builders also multiply reach: one integration covers every agent on their platform.

## Consequences

- The roadmap leads with agents: online scoring of agent traces in Phase 1, offline agent runs with harnesses and sandboxes in Phase 2.
- agentgateway integration comes early, in Phase 1 for standalone.
- The API and policy-as-code are first-class for platform builders: everything the CLI does is an API call.
- Classic ML packs (`ml-classic`, `ml-monitoring`) move to Phase 5 unless a client needs them sooner. RL stays in Phase 4.
