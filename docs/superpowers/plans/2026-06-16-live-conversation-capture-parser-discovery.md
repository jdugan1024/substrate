# Live Capture Parser Discovery Notes

## Codex

Observed in yolobox:

- `/home/yolo/.codex/history.jsonl` contains prompt history records with `session_id`, `ts`, and `text`.
- `/home/yolo/.codex/state_5.sqlite`, `goals_1.sqlite`, `logs_2.sqlite`, and `memories_1.sqlite` exist.
- `sqlite3` was not installed in the yolobox image during planning, so table schemas were not inspected.

Next host-side command:

```bash
sqlite3 ~/.codex/state_5.sqlite '.tables'
sqlite3 ~/.codex/logs_2.sqlite '.tables'
sqlite3 ~/.codex/state_5.sqlite '.schema'
sqlite3 ~/.codex/logs_2.sqlite '.schema'
```

Acceptance criterion for a Codex parser:

- It must emit both human and assistant messages for a session.
- If only prompt history is available, do not enable the parser by default because it would create misleading one-sided transcripts.

## Zed

No Zed store was visible inside yolobox. Run on the host:

```bash
find ~/.config ~/.local/share -maxdepth 5 \( -iname '*zed*' -o -path '*zed*' \) 2>/dev/null
```

Acceptance criterion for a Zed parser:

- Prefer SQLite or JSON stores over UI cache files.
- Add a fixture copied from a single redacted thread before writing parser code.

## Pi

No Pi store was visible inside yolobox. Run on the host:

```bash
find ~ -maxdepth 6 \( -iname '*pi*' -o -path '*pi*' \) 2>/dev/null
```

Acceptance criterion for a Pi parser:

- Identify a durable local transcript source containing user and assistant text.
- Add a redacted fixture before writing parser code.
