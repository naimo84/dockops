# Dockops Self-Update (Phase 12) — Design

Status: Entwurf, 26.10.2026

## Problem

Alles, was dockops deployt, kann es auch aktualisieren — nur sich selbst nicht. Der
Apply-Pfad (`internal/reconciler/apply.go`) fährt `compose pull` + `compose up` als
**Kindprozess im eigenen Container**. Ersetzt dieser Up den eigenen Container, stirbt die
ganze cgroup: kein Health-Gate, kein Rollback, kein State-Write, keine Log-Zeile. Deshalb
liegt der dockops-Stack heute noch bei Komodo.

## Entscheidung

**Selbst-Update über einen detached Helper-Container.** Der Agent schreibt einen Plan,
startet einen kurzlebigen Container mit eigenem Lebenszyklus (Geschwister, kein Kind) und
beendet seinen Teil des Passes. Der Helper recreated, prüft die Gesundheit und rollt bei
Bedarf zurück. Das Ergebnis landet als Datei im State-Dir und wird beim nächsten Start
finalisiert.

Verworfen:
- Watchtower-Klon / zweiter Updater mit Socket-Rechten: zweite Angriffsfläche, zweiter Manager.
- `restart: unless-stopped`-Trick: ein Restart startet das **alte** Image, kein Recreate.
- Recreate per Host-Systemd: der Container kommt an kein dbus, ohne hässliche Mounts.

## Erkennung

Ein Stack ist **self**, wenn `stack.Name == selfProject` mit `selfProject` aus
`DOCKOPS_SELF_PROJECT` (Default `dockops`). Das ist derselbe Wert, den `/status` schon
benutzt, um den eigenen Container nicht als unmanaged zu melden — keine zweite Quelle.

Zusätzliche Absicherung: `DOCKOPS_SELF_UPDATE=off` schaltet den Pfad komplett ab (dann
verhält sich ein Self-Stack wie jeder andere — und genau deshalb ist das nur für Debug
gedacht).

## Ablauf im Apply-Pfad

`applyUpdates` prüft nach dem `Pull` und **vor** dem `Up`, ob der Stack self ist:

1. Kandidaten wie gehabt bestimmen, `compose pull` (unschädlich, ersetzt nichts).
2. Für jeden Kandidaten: `PreviousDigest = CurrentDigest`,
   `ApplyPendingDigest = RemoteDigest`, `ApplyError = ""`, `PendingAt = now`.
3. State persistieren (der sterbende Prozess hat jetzt einen ehrlichen Zwischenstand).
4. Plan-Datei schreiben (JSON, siehe unten) in `stateDir/selfupdate-plan.json`.
5. Helper starten: `docker run -d --rm --name dockops-selfupdate-<unix> …`.
6. Log INFO `self update: helper started`, Event `update_available`-artige Notiz **nicht** —
   der Erfolg wird erst finalisiert. `applyUpdates` gibt `nil` zurück; der laufende
   Container lebt bis zum Recreate des Helpers weiter.

Kein in-process Rollback: der Prozess, der ihn fahren müsste, ist der, der ersetzt wird.

## Plan-Datei (`selfupdate-plan.json`, Version 1)

```json
{
  "version": 1,
  "project": "dockops",
  "stack_dir": "/var/lib/dockops/repos/host/dockops",
  "compose_files": ["compose.yaml"],
  "env_file": "/var/lib/dockops/repos/host/dockops/.env",
  "services": ["dockops"],
  "containers": ["dockops"],
  "grace_seconds": 10,
  "health_timeout_seconds": 180,
  "health_poll_seconds": 3,
  "targets": [
    {"image": "ghcr.io/naimo84/dockops:latest", "repository": "ghcr.io/naimo84/dockops",
     "digest": "sha256:…", "previous": "sha256:…", "service": "dockops"}
  ]
}
```

`containers` ist die Health-Gate-Zielliste (Container-Namen, nicht Service-Namen: der
Healthcheck hängt am Container). Fehlt ein Name, fällt der Helper auf den Service zurück.

## Helper (`dockops -selfupdate-helper -plan <pfad>`)

Läuft im eigenen Image (docker CLI + compose-Plugin sind drin), als eigener Container mit
`/var/run/docker.sock`, dem Stack-Verzeichnis am **gleichen Pfad** und dem State-Dir:

1. Plan lesen, `grace_seconds` schlafen (der Agent soll seinen Pass abschließen können).
2. `docker compose -p <project> -f … --env-file <env> up -d --no-deps <services>`.
3. Health-Gate: `docker inspect` je Ziel-Container, bis `healthy` (oder `running` ohne
   Healthcheck), Timeout `health_timeout_seconds`, Poll `health_poll_seconds`.
4. Erfolg → Ergebnis `{"status":"ok", "digest": <target-digest>, "at": …}`.
5. Fehler → Rollback-Override (`<repository>@<previous>` je Service, gleiche Form wie
   `rollbackFilePattern` im Agent), erneut `up -d --no-deps`, zweites Health-Gate,
   Ergebnis `{"status":"rolled_back"|"rollback_failed", "error": …, "at": …}`.
6. Ergebnis atomar schreiben (`stateDir/selfupdate-result.json`, tmp+rename) und beenden.
   `--rm` räumt den Helper ab; Logs gehen zusätzlich nach `<stateDir>/selfupdate-helper.log`,
   weil der Helper nach einem gescheiterten Recreate die einzige Quelle ist.

## Finalisierung (`internal/reconciler/selfupdate.go`)

Beim Pass-Start, vor dem Reconcile, `finalizeSelfUpdate(ctx)`:

- Ergebnis-Datei vorhanden:
  - `ok` → `CurrentDigest = digest`, `AppliedAt = now`, `ApplyPendingDigest = ""`,
    `UpdateAvailable = false`, `ApplyError = ""`; Event `update_applied`; Datei löschen.
  - `rolled_back` → `ApplyBlockedDigest = digest`, `ApplyError = error`,
    `ApplyPendingDigest = ""`, `RolledBackAt = now`; Event `update_failed`; Datei löschen.
  - `rollback_failed` → wie `rolled_back`, zusätzlich Text mit `rollback FAILED`
    (dieselbe Markierung, die der In-Process-Pfad nutzt); Datei löschen.
- Kein Ergebnis, aber `ApplyPendingDigest` gesetzt **und** laufender Digest ==
  `ApplyPendingDigest` → der Helper ist gestorben, das Recreate hat aber funktioniert:
  wie `ok` behandeln (Log WARN `self update: no result file, digest matches target`).
- Kein Ergebnis, `ApplyPendingDigest` gesetzt, laufender Digest == `PreviousDigest` →
  als fehlgeschlagen werten (`ApplyBlockedDigest = ApplyPendingDigest`), damit der Pass
  nicht in einer Endlosschleife dasselbe Recreate versucht.

Beim Lesen des eigenen Digests gilt dieselbe Quelle wie im Update-Check
(`compose.Images` / `ImageDigest`), nicht `docker inspect` am eigenen Container.

## State

Neues Feld in `state.ImageRecord`:

```go
// ApplyPendingDigest ist der Ziel-Digest eines laufenden Self-Updates: der Agent
// hat den Helper gestartet und kann das Ergebnis nicht mehr selbst verbuchen.
ApplyPendingDigest string    `json:"apply_pending_digest,omitempty"`
PendingAt          time.Time `json:"pending_at,omitempty"`
```

Kein Schema-Bump (`fileVersion` bleibt 1): unbekannte/fehlende Felder sind alte Stände.

## /status

`updates[]` bekommt `pending: true`, solange `ApplyPendingDigest` gesetzt ist. Kein neues
Top-Level-Feld.

## Grenzen (bewusst)

- **Ein Self-Stack pro Host.** Mehrere dockops-Instanzen auf einem Host sind kein Fall.
- **Kein paralleler Reconcile-Pass:** der Helper arbeitet asynchron; der Pass endet vor dem
  Recreate. Solange der Helper läuft, kann ein zweiter Pass (Poll/Webhook) den Plan sehen und
  muss ihn ignorieren (`ApplyPendingDigest` gesetzt → kein neuer Plan).
- **Image des Helpers = das laufende Image.** Ein kaputtes neues Image kann sich also nicht
  selbst helfen; deshalb der Rollback auf `previous` im Helper.
- **Der Helper hat Socket-Rechte.** Er ist derselbe Container wie der Agent, nur kurzlebig und
  ohne Repo-/Secret-Zugriff (State-Dir nur lesend für den Plan, schreibend nur für Ergebnis/Log).
