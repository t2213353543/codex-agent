# Fixed model mapping — 0.4.12 candidate

The user requested a verified, hardcoded routing table exposed in settings. Generation requests select exactly one target from `bps_model_routes` locally. They do not fetch `/responses/access`, probe model availability, or try different models after an error. Existing account failover still uses the same selected model and preserves account concurrency, weight and priority.

The management setting uses one `source=target` rule per line. Blank lines and # comments are allowed; duplicate sources, unsupported targets and missing `*` fallback are rejected. The defaults include six supported model self-mappings, known old aliases and `*=gpt-6-astra`. An explicit source rule wins, otherwise `*` selects the final target; there is no recursive alias chain. Administrators can redirect a model to a verified higher or lower target by changing the table.

The six targets and their reasoning levels were verified once through the management model list in `.local/model-fallback-release-20260928/static-model-discovery.json`: gpt-5.6-luna/terra/sol and gpt-6-luna/sol support none, low, medium, high, xhigh; gpt-6-astra supports low, medium, high, xhigh. Defaults map gpt-5.5 and known full Codex aliases to gpt-5.6-sol, codex-auto/review to gpt-6-astra, mini/spark aliases to gpt-5.6-luna. Unmatched names map to the configured fallback. `max`/`ultra` become xhigh; exact supported levels are retained, other known levels choose a higher supported level first and then a lower one. Auto aliases with no requested effort use the target's highest level. Other requests with no effort retain the upstream default.

Settings are versioned and persisted. Invalid updates leave the saved table intact; omission by an older client preserves it. A one-version immutable parsed-table cache avoids repeated parsing without retaining unbounded configuration versions. No upstream calls are made by this cache.

Model/effort rewrites preserve all other request bytes. Request history keeps the original model; actual cache scope uses the sent model. Logs distinguish requested, sent and returned models and requested/sent reasoning. Responses and usage remain upstream observations, not rewritten claims.

Old dynamic catalog experiments and their test reports are historical evidence, not the current generation contract. Final 0.4.12 source/live/release/postdeploy evidence is under `.local/model-fallback-release-20260928/`. A passing candidate or document alone does not prove deployment.
