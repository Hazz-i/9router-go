### fix(codebuddy): read the `msg` field so "request illegal" backs the account off

- CodeBuddy answers a failed channel verification with `{"code":11128,"msg":"request illegal"}`, a body with no `message` field. `extractErrorText` read only `error.message`, the top-level `message`, and `error.details[].reason`, so that body extracted to `""`: the `codebuddy-intl` text rule could never match, a 403 fell through to the status rule's fixed two-minute cooldown, the account-level backoff level never escalated, and the stored `lastError` the dashboard shows read `{"status":403,"message":""}`.
- The extractor now falls back to the top-level `msg` after `error.message` and `message`, so the provider-scoped rule fires, the connection takes the exponential backoff, and the dashboard names the reason.
- Pinned by `TestExtractErrorText_CodeBuddyMsgField` (four cases: the CodeBuddy envelope, and that `error.message` / top-level `message` keep their precedence).

### fix(caption): send the self-hosted transcription POST through the connection's proxy

- Live caption's three WebSocket backends already dial through the chat lane's proxy chain, but the self-hosted backend is the one path that sends HTTP: `postSelfhosted` used the handler's plain `http.Client`, so a connection bound to a proxy pool still posted the audio from the gateway's own address while the dashboard showed it as proxied.
- The handler now resolves `chatH.GetClientForConnection` (the same chain: assigned pool, then the legacy per-connection proxy) and the session reuses that client for every flush. A pool that cannot serve the request is refused and the flush dropped — never a silent direct dial.
- An edge-relay pool is refused for this lane specifically: a relay forwards by rewriting the request onto `x-relay-target` / `x-relay-path`, which only the chat lane implements, so using it here would post to the real base URL.
- Pinned by `TestSelfhostedTranscriptionUsesConnectionProxy`: the POST arrives at the proxy with the absolute upstream URL and the direct upstream sees zero requests; a relay pool drops the flush with a recorded reason.
