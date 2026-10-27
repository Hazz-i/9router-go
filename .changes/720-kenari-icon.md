### feat(image): Kenari shows its own mark

- `web/public/providers/kenari.svg` is now Kenari's own identity — the k-mark from `https://kenari.id/favicon.svg` (cream tile, ink lowercase "k", brick square accent) — instead of the placeholder camera glyph added with the provider.
- `getIconPath` only ever asked for `/providers/<id>.png`, so a provider that ships an SVG (Kenari) answered 404 on every tile and fell back to the initials badge; the artwork existed but was never requested. An `SVG_ARTWORK` allowlist now returns the `.svg` path for those ids, and the existing `<img onerror>` fallback still covers a missing file. Codewhale/smelt ship SVGs too but have no catalog entry, so they never reach this path.
- The catalog entry now carries the brand colour (`#B5362A`, Kenari's brick) and a notice that names the API-key page, so the initials fallback and the provider card read as Kenari rather than an anonymous endpoint.

Verified: `bun test src scripts` (660 pass, including the new getIconPath cases for the SVG allowlist and two PNG providers left unchanged), `bun run build`, svelte-check ratchet (0 unresolved, 77 = baseline), `bunx oxlint --deny-warnings`, and live on a local gateway: `GET /providers/kenari.svg` answers 200 `image/svg+xml` (486 bytes) and the served bundle asks for `kenari.svg`.

Note: the SVG renders the "k" with `font-family: Archivo, Helvetica, Arial, sans-serif`, so a machine without Archivo draws the same letter in a system bold face — the geometry (tile + brick accent) is unchanged.
