# Relative Bind-Quellen in deployed Compose-Dateien — Design

Status: Entwurf, 10.11.2026 (Issues #18 und #23)

## Problem

dockops läuft als Container und startet Compose mit dem **Container-Pfad** der
compose-Datei. Relative Bind-Quellen (`./config.yaml:/app/config.yaml:ro`)
löst Compose gegen das Verzeichnis der compose-Datei auf — also gegen
`/var/lib/dockops/repos/<remote>/<stack>`. Der Docker-Daemon bekommt diesen
Pfad und löst ihn in **seinem** Namespace auf, dem Host, wo er nicht existiert.
Docker legt ein leeres Verzeichnis an und mountet es auf das Datei-Ziel:

- `read /etc/traefik/config.yaml: is a directory` (traefik, 4 Minuten Ingress-Ausfall)
- `A storage backend must be specified` (vault, RestartCount 11)
- `IsADirectoryError: [Errno 21] Is a directory: '/app/config.yaml'` (litellm)
- `env file …/.env.default not found` beim Image-Update-Check (dieselbe Ursache)

Kein Deploy-Fehler — nur ein Container, der später kippt. Der bisherige
Workaround (alle Quellen absolut schreiben) funktioniert nur, solange Host- und
Container-Pfad identisch sind, und ist unsichtbar falsch, sobald jemand die
Mounts umhängt.

## Ziel

Relative Bind-Quellen funktionieren wie auf dem Host. Eine nicht auflösbare
Quelle ist ein lauter Fehler im Pass statt eines stillen Leer-Mounts.

## Entscheidungen

### 1. Rewrite statt Identitäts-Mounts

Variante 1 aus #18 (Checkouts so mounten, dass Container- und Host-Pfad
identisch sind) verlagert das Problem in jede `compose.yaml` des Host-Repos und
in die Doku jedes Hosts. Der Rewrite löst es im Agenten, für alle Hosts
gleichzeitig, ohne Betriebsänderung.

### 2. Generiertes Override als zusätzliches `-f`

`<stackDir>/dockops-binds-<stack>.yaml`, nach den Stack-Dateien übergeben.
Compose merged Volumes über `target`, das spätere File gewinnt — der
absolute Host-Pfad ersetzt genau die relative Quelle, alle anderen Mounts
bleiben. Ein reines Umschreiben der compose-Datei im Checkout ist keine
Option: der Checkout ist maschinell verwalteter Git-Zustand (`reset --hard`,
`clean -fd`), und der Stack-Hash würde bei jedem Deploy wandern.

Die Datei wird atomar geschrieben (`.tmp` + rename) und nach dem Deploy
entfernt; ein liegengebliebenes Artefakt nach einem Absturz überschreibt der
nächste Deploy bzw. `git clean -fd` räumt es ab.

### 3. Mount-Zuordnung aus `docker inspect` des eigenen Containers

`internal/compose` liest `docker inspect <self> --format '{{json .Mounts}}'`
einmal pro Deploy (nicht pro Pass; die Zuordnung ändert sich nur beim Recreate
des Agenten). Der **tiefste** passende Mount gewinnt: `/var/lib/dockops`
(State-Volume) und `/var/lib/dockops/repos/host` (Checkout-Bind) liegen
ineinander, ein flacher Treffer würde in den falschen Namespace übersetzen.

Ohne Mounts (Agent läuft direkt auf dem Host) ist der Container-Pfad schon der
Daemon-Pfad: die Quelle wird absolut geschrieben, aber nicht übersetzt.

Fehlt der eigene Container (kein `os.Hostname()`), wird gewarnt und ohne
Übersetzung deployt — nur der nicht übersetzbare Fall ist ein harter Fehler.
Die Warnung ist der ehrlichere Weg als ein Deploy, der ohne Not ausfällt.

### 4. Nur relative Quellen, nur Binds

Absolute Quellen werden **nicht** validiert: Wer sie deklariert, kennt den
Host-Namespace; eine Prüfung würde jeden adoptierten Stack brechen, dessen
Host-Pfad vom Container-Pfad abweicht. Named Volumes haben keinen Host-Pfad und
sind per Konstruktion portabel (`looksLikePath` trennt beide Formen).

### 5. Harte Fehler nur bei Unauflösbarem

Liegt die aufgelöste Quelle außerhalb aller Mounts, bricht der Deploy vor dem
ersten Compose-Aufruf ab — mit Quelle, `target` und aufgelöstem Pfad. Das ist
der eine Fall, den dockops sicher als kaputt erkennen kann; ein stiller
Leer-Mount ist genau das Fehlerbild, das die Änderung abschafft.

### 6. Helper bekommt die Zuordnung als Argument

Der Phase-13-Helper erbt die Mounts des Agenten (`--volumes-from` + `-v`) und
führt denselben Rewrite aus. Die Zuordnung kommt als
`-selfupdate-mounts container=host;…` mit (argv statt Env: der Agent ist der
einzige Starter, und die Argumentliste ist schon der Vertrag zwischen beiden
Seiten — vgl. den `-selfupdate-plan`-Drift, der live zwei Stunden gekostet
hat).

## Nicht im Scope

- Validierung absoluter Host-Pfade (`/status`-Sichtbarkeit aus #18, Vorschlag 3)
- Automatisches Umschreiben der Host-Repos auf Identitäts-Mounts
- Das `compose config`-Env-Problem im Image-Update-Check (Nebenfund #18): mit
  einem relativen `env_file: ./.env.default` löst es sich über denselben
  Rewrite nicht auf, weil `env_file` kein Bind ist — eigener Fix.

## Tests

`internal/compose/binds_test.go` (Rewrite-Regeln: Übersetzung, tiefster
Mount, Host-Modus, `-f`-Merge, Long-Syntax, Refusal), `binds_exec_test.go`
(Executor-Pfad: `-f`-Reihenfolge, Cleanup, Refusal vor Compose, `OwnMounts`),
`cmd/dockops/binds_test.go` (Helper-Deploy: Rewrite, Refusal-als-Result,
`-selfupdate-mounts`-Roundtrip).
