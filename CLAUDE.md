# dockops

Deklarativer Docker-Compose-Stack-Manager: `stacks.yaml` im Host-Repo-Root
beschreibt lokale Stacks (rekursiv via Include-Globs) und Import-Stacks aus
Remote-Checkouts unter `global.work_dir`. Phase 1: Go-Bootstrap und
YAML-Config-Loader (`internal/config`).

## Umgebung

- Go 1.25, Toolchain unter `~/.local/go/bin/go` — vor jedem go-Aufruf:
  `export PATH=$HOME/.local/go/bin:$PATH`
- Module-Pfad: `github.com/local/dockops`

## Struktur

- `cmd/dockops/` — CLI-Entry-Point
- `internal/config/` — Config-Loader: Root-Config, Stack-Discovery über
  Include-Globs, Compose-Auto-Detection, Import-Merge, Validierung
- `PHASE1_TASK.md` — Aufgabenstellung (nicht ändern)

## Konventionen

- Logging ausschließlich über stdlib `log/slog` (kein anderes Log-Paket).
- Tests mit stdlib `testing`, kein testify. Fixtures über `t.TempDir()`
  (Host-Repo + Fake-Remote-Checkouts als Temp-Verzeichnisse aufbauen).
- YAML: `gopkg.in/yaml.v3` (einzige externe Abhängigkeit), Strict-Decoding
  mit `KnownFields(true)`, damit Tippfehler als Load-Fehler auffallen.
- Kein `panic` in Library-Code: alle Fehler als `error` zurückgeben.
- Validierungsfehler enthalten immer Kontext (Datei, Pfad, Feld, Index).
- Alle exportierten Symbole sind mit Doc-Kommentaren dokumentiert.
- Offene Punkte im Briefing werden konservativ entschieden (stdlib, klare
  Fehler) und die Entscheidung als Kommentar im Code festgehalten.

## Checks vor jedem Commit

    export PATH=$HOME/.local/go/bin:$PATH
    go build ./... && go vet ./... && go test ./...
