# Translating AniCLI / Перевод AniCLI

AniCLI's interface is multilingual: the bundled tables are `en.toml`
(the source of truth) and `ru.toml`. Community-contributed languages
are first-class: a user drops `ja.toml` into
`~/.config/anicli/locales/`, sets `[general] locale = "ja"` in
settings.toml, and the whole interface switches — missing keys
fall back to English automatically.

## Format rules

- One flat table: every line is `"dotted.key" = "value"` — quoted keys,
  namespace prefixes group strings by screen (`menu.*`, `search.*`,
  `session.*`, …).
- `{placeholders}` (for example `{count}`, `{err}`) are substituted by
  the code at render time. Keep them verbatim; you may reorder them or
  adjust surrounding words freely.
- Values may contain emoji and Unicode punctuation; keep the tone
  compact — most strings render on status lines.
- A key present in `en.toml` must be present in every bundled table
  (`TestBundledParityENRU` enforces this for ru; user files may be
  sparse — missing keys fall back to `en`).

## Contributing a new language

1. Copy `locales/en.toml` to `locales/<lang>.toml` (BCP-47 style short
   code: `ja`, `de`, `pt-BR`, …).
2. Translate the values. Do not rename keys.
3. Test locally without a rebuild: copy the file to
   `~/.config/anicli/locales/<lang>.toml`, set
   `[general] locale = "<lang>"` and run `anicli`.
4. Open a pull request adding the file to `locales/`. Bundled
   languages ship inside the binary for every user.

## Overriding a bundled language

A user file with the same name overrides the bundled table key by key:

    ~/.config/anicli/locales/ru.toml

Only the overridden keys need to be present; everything else resolves
from the bundled table, then from `en.toml`, then renders as the key
itself (so a typo is visible on screen instead of failing silently).
