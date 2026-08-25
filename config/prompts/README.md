# Prompt sections directory

Each file named `NNN-name.md` becomes one system-prompt section:

- `NNN` is the section's order (sections render in ascending order).
- `name` is the section's name (used to add/remove it programmatically).
- The file body is the section text; empty sections are skipped in output.

Current sections:

| file                 | section   | note                              |
|----------------------|-----------|-----------------------------------|
| `10-persona.md`      | persona   | the agent's persona               |
| `20-skills.md`       | skills    | optional skill guidance            |

Files without the `NNN-` prefix (like this README) are ignored, so documentation
can live here. To add a section, add a file (e.g. `15-examples.md`) — no code
change. To remove one, delete its file.
