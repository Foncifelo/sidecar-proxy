Contract recordings per provider live here: <provider>/chat.json, <provider>/chat.stream.sse. Scrub keys before committing; CI runs gitleaks.

FICTICIO: every file currently in this directory is fictitious, hand-written to
mirror the OpenAI-compatible wire format. Ids, models, token counts and the
"provider" values are invented, not recorded from a real API. Replace them
with real scrubbed recordings via `make record`.
