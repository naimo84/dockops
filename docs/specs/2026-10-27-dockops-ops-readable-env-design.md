# Ops-lesbares Stack-Env (Phase 14)

Datum: 2026-10-27 · Status: Entwurf · Vorgänger: Phase 13 (Deploy-Intercept)

## Problem

Jeder Deploy schreibt `<stackDir>/.env` (`env.WriteStackEnv`) mit dem gemergten
Environment — inklusive Secrets. Die Datei entsteht `0600` und gehört dem
uid des Agenten (auf der Fleet `1000:4001`).

Der host-seitige Weg `deploy <stackdir> up` läuft dagegen als **anderer**
Benutzer (`hermes`, uid 3200). Er braucht die Datei, weil die Compose sie per
`env_file: ./.env` einliest und `${...}` daraus interpoliert. Ergebnis:

```
open /var/lib/dockops/repos/infrastructure/dockops/.env: permission denied
```

Damit ist der einzige host-seitige Rettungsweg — Bootstrap eines neuen Images
und Rollback ohne laufenden Agenten — für den Betriebsbenutzer dicht. Der
bisherige Workaround war ein temporäres `chmod 644` per `docker exec`: er
macht Secrets weltlesbar, also schlimmer als das Problem. Phase 13 hat den
Helper-Weg geliefert; dieser hier schließt den *menschlichen* Weg.

## Lösung

Mode und Gruppe des generierten `.env` werden konfigurierbar. Angewendet wird
beides **vor** dem Rename auf die Temp-Datei, damit die Datei unter ihrem
Zielnamen nie in einem Zwischenzustand existiert.

```yaml
# stacks.yaml (Host-Repo, global)
env_file:
  mode: "0640"      # Default "0600" — unverändertes Verhalten
  group: "docker"   # Name oder numerische gid; leer = nicht anfassen
```

Globale statt per-Stack-Konfiguration: das ist eine **Betriebs**-Eigenschaft
des Hosts (wer darf den Stack von Hand deployen), keine Eigenschaft des
einzelnen Stacks — sonst müsste jeder Stack, der host-seitig deploybar sein
soll, denselben Block wiederholen.

`0640` + Gruppe `docker` genügt auf der Fleet, weil der Ops-Benutzer dort
Mitglied von `docker` ist (er muss ohnehin an den Socket) — und der Agent ist
es über sein `group_add` ebenfalls, kann also `chgrp` ausführen.

### Validierung (Config-Loader, hart)

- `mode`: Oktalstring, mit oder ohne führende `0` (`"640"` == `"0640"`).
- **Weltlesbar wird abgelehnt** (`mode & 0o004`): die Datei trägt Secrets, und
  genau dieser Fehler ist der Anlass der Phase. Klarer Config-Fehler beim
  Laden statt stillem Downgrade.
- Setuid/Setgid/Sticky abgelehnt (`mode &^ 0o777`).
- `group`: keine Vorab-Auflösung im Loader (der läuft in Umgebungen ohne
  passende `/etc/group`); Auflösung passiert beim Schreiben.

### Fehlerverhalten beim Schreiben

Beides **Warnung, kein Abbruch** — ein Deploy darf nicht an einer
Berechtigungsnuance des Artefakts scheitern:

- Gruppe nicht auflösbar (Tippfehler, Host ohne diesen Eintrag): Warnung mit
  Stack-Pfad, Gruppenwert und Hinweis, Datei entsteht ohne `chgrp`.
- `chgrp` verweigert (`EPERM`, Agent ist kein Mitglied): Warnung mit beiden
  gids; Datei entsteht mit dem Mode.

Die Warnung ist die Diagnose: sie nennt den *Grund*, warum der Ops-Benutzer
die Datei später nicht lesen kann.

### Env-Overrides

`DOCKOPS_ENV_FILE_MODE` / `DOCKOPS_ENV_FILE_GROUP`, Präzedenz wie bei
`host_checkout`: **YAML gewinnt**, die Variable füllt nur die Lücke. Für
Hosts, deren `stacks.yaml` nicht angefasst werden soll.

## Nicht im Scope

- Per-Stack-Override (siehe Begründung oben).
- ACLs statt Gruppe — mehr Maschinerie für denselben Effekt.
- Rechte anderer Artefakte (`state.json`, Plan/Result/Helper-Log bleiben
  `0600`; sie sind Agent-intern).
- Der Wrapper (`hermes-ops`) und die Ansible-Playbooks.

## Betroffene Stellen

| Stelle | Änderung |
|---|---|
| `internal/config/types.go` | `Global.EnvFile EnvFile` (`mode`, `group`) |
| `internal/config/load.go` | Defaults, Parsing, Validierung, Env-Override |
| `internal/env/env.go` | `WriteStackEnv(stackDir, env, opts)`; `chmod`/`chgrp` auf die Temp-Datei vor dem Rename; Warnungen; `Options`-Typ + Normalisierung |
| `internal/reconciler/reconciler.go` | `Deps.EnvFileOptions` an `WriteStackEnv` durchreichen |
| `cmd/dockops/main.go` | Options aus `Global` bauen (Env-Override, YAML-Vorrang) |
| README, CLAUDE.md | Betriebsabschnitt, Grenzen; `chmod 644`-Workaround als obsolet markieren |

## Tests

Alle neuen Tests in eigenen Dateien (`phase14_*_test.go`), die bestehenden
Env-Tests bleiben unverändert — der Default muss beweisbar `0600` bleiben.

1. Ohne Optionen: `0600` (Regression, bestehende Tests decken es).
2. `mode: 0640`: Datei hat `0640`, Inhalt identisch, kein Temp-Rest im
   Verzeichnis.
3. `group` als numerische gid (eigene egid, um `EPERM` zu vermeiden):
   `st_gid` stimmt.
4. `group` als Name (Name der eigenen egid): dito.
5. Unbekannte Gruppe: Warnung, Datei existiert, kein Fehler.
6. `chgrp` scheitert (`EPERM`, z. B. gid 0 als Nicht-root): Warnung, kein
   Fehler; Test überspringt sich als root.
7. Config: gültige Modi (`"640"`, `"0640"`, `"0600"`), ungültig (`"64x"`,
   leer), weltlesbar (`"0644"`, `"0604"`) → Fehler, Setuid (`"04640"`) →
   Fehler.
8. Env-Override füllt Lücke; gesetztes YAML gewinnt.
9. Rollback nach fehlgeschlagenem Deploy stellt den vorherigen Mode wieder
   her (bestehende Rollback-Tests bleiben grün).

DoD: `gofmt -l` leer, `go build ./...`, `go vet ./...`,
`go test ./... -count=1` grün, PASS ≥ 685.

## Live-Abnahme

1. Merge → CI → Image.
2. Host-Repo `docker/devbox`: `env_file: {mode: "0640", group: "docker"}` in
   `stacks.yaml` (eigener PR).
3. Deploy auf devbox; danach als Ops-Benutzer ohne `chmod`-Trick:
   `deploy /home/docker/infrastructure/dockops up` → kein
   `permission denied`.
4. `ls -l` der `.env`: `-rw-r----- naimo docker`.
