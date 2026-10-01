# AGENTS.md

External plugins for [PaperValet](https://github.com/PaperValet/PaperValet). Each plugin is a Go plugin (`.so`) in its own directory under `plugins-external/<name>/` with its own `go.mod`.

Read the main repo's [AGENTS.md](https://github.com/PaperValet/PaperValet/blob/master/AGENTS.md) for shared conventions (bilingual text, Markdown escaping, small commits, no compatibility shims).

## Build and test

Use the build-plugin skill from the main repo. Keep a PaperValet checkout next to this repo (`../PaperValet`), then:

```bash
../PaperValet/.agents/skills/build-plugin/scripts/build-plugin.sh plugins-external/<name>
```

It runs gofmt, vet and tests, builds with the pinned Go version inside a `go work` workspace, and proves the `.so` loads into the current PaperValet. Read [its SKILL.md](https://github.com/PaperValet/PaperValet/blob/master/.agents/skills/build-plugin/SKILL.md) before touching build settings or debugging a load failure.

Never commit `go.work`, `go.sum` or `.so` files (all gitignored).

## Plugin checklist

- `var Metadata = &plugin.PluginMetadata{...}` with `Name` matching the directory and `Name()`, plus `Description`, `DescEN`, `Version`, `Author`. `scripts/gen-registry.py` turns it into `plugins.json`, which `apt` reads.
- Every command sets `Description`, `DescEN`, `Usage`, `UsageEN`, and answers `<command> help`.
- No `Aliases` except one short form for a long command (`ddg`, `st`), and one spelling per subcommand (`list`, not `list`/`ls`). Users make shortcuts with `.alias`.
- Persistent state in `data/<name>/` via `mgr.Host().DataDir`. Goroutines stop in `Stop`.
- Only import `github.com/TiaraBasori/PaperValet/pkg/plugin`. If the SDK lacks something, add it to PaperValet first.
- Tests cover parsing and pure logic; nothing in tests touches the network.

## Adding or removing a plugin

- Add: new directory, then add a row to the table in `README.md`.
- Remove: delete the directory and its README row, then delete the stale asset from the rolling release: `gh release delete-asset latest <name>.so -R PaperValet/PaperValet-Plugins -y`.

## Release

Every push to `main` makes CI (`plugins-release.yml`) build all plugins against PaperValet master and republish the `latest` release with each `.so` and `plugins.json`. If PaperValet's SDK changed, push PaperValet first so this build picks it up.
