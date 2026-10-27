# Dockops Deploy-Pfad-Intercept (Phase 13) — Design

Status: Entwurf, 27.10.2026

## Problem

Phase 12 hat den Self-Update (Image-Apply) über einen detached Helper-Container
gelöst. Der Intercept sitzt aber in genau **einem** Pfad
(`internal/reconciler/apply.go`, `applyUpdates`). Jeder andere Weg, der den
eigenen Container ersetzt, läuft weiter in-process und killt den Pass:

- **Hash-/Config-Deploy** des Self-Stacks (geänderte Compose, neue Env-Variable,
  neuer Service, Profile-Wechsel) — `deployStack` ruft `compose.Up` im eigenen
  Container.
- **Erst-Adoption**: der Self-Stack wird beim ersten Pass überhaupt deployt.
  Das ist der häufigste Fall, sobald man ihn in die Config aufnimmt.

Bei `container_name:` in der Compose muss compose den alten Container
**entfernen, bevor** es den neuen erstellt. Stirbt die CLI in diesem Fenster,
ist der Stack unten: `restart:` greift nicht (der Container ist weg), die
systemd-Unit feuert nur bei Boot/Start. Deshalb ist die Adoption gefährlicher
als der laufende Betrieb.

Solange dieser Pfad offen ist, darf der Self-Stack nicht in die Config — d. h.
die Phase-12-Arbeit ist ohne Phase 13 nicht produktiv nutzbar.

## Ziel

Jeder Recreate des Self-Stacks geht über den detached Helper. Der Agent
schreibt nur noch Plan + State und gibt ab; er führt für den Self-Stack kein
`compose up` mehr aus.

## Entscheidungen

### 1. Plan bekommt `kind` — Schema bleibt Version 1

`Plan.Kind` (`json:"kind,omitempty"`): leer = `images` (Phase 12,
rückwärtskompatibel), `deploy` = Voll-Deploy.

**Version bleibt 1.** Grund: beim Self-Update schreibt das *alte* Image den
Plan, der Helper läuft aber schon mit dem *neuen* Image. Eine Versionserhöhung
würde genau diesen Übergang brechen (neuer Helper, alter Plan → reject → kein
Update mehr möglich). Neue Felder sind deshalb additiv und `omitempty`; die
Semantik „leeres Feld = Phase-12-Verhalten" ist die Migrationsregel.

Neue Felder:

- `Kind string` — siehe oben.
- `Profiles []string` — die Profile, mit denen `compose up` läuft
  (`upSpec.profiles`); ohne sie fehlen Profil-Services im Deploy.
- `DeployHash string` — der Stack-Hash, der nach Erfolg als deployed gilt.
  Nur bei `kind=deploy`.

`Validate`:

- `kind=images`: wie bisher (`services` und `targets` nicht leer).
- `kind=deploy`: `targets` **muss** leer sein (ein Voll-Deploy hat keinen
  Rollback-Digest), `deploy_hash` nicht leer, `services` **darf** leer sein
  (leer = „alle Services", also `compose up` ohne Service-Argumente — genau
  wie `compose.Up` bei `len(upServices)==0`).
- unbekannter `kind` → Fehler.

### 2. State: Deploy-Pending auf Stack-Ebene

`state.State` bekommt zwei additive, `omitempty` Felder (fileVersion bleibt 1):

- `DeployPendingHash string` (`deploy_pending_hash`) — der Hash, dessen Deploy
  an den Helper übergeben wurde. Solange gesetzt, startet kein Pass einen
  zweiten Helper und `deployStack` fasst den Stack nicht an.
- `DeployPendingAt time.Time` (`deploy_pending_at`) — Zeitpunkt der Übergabe;
  Basis für den Alters-Timeout in der Finalisierung.

Warum auf `State` und nicht auf `ImageRecord`: ein Deploy betrifft den Stack,
nicht ein Image. `PendingAt`/`ApplyPendingDigest` bleiben unverändert für den
Image-Apply-Pfad.

### 3. Ablauf in `deployStack` (nur Self-Stack, nur mit `DOCKOPS_SELF_UPDATE=on`)

Der Guard sitzt **vor** jedem `compose.Up` und nach Hash-Berechnung:

1. `st.DeployPendingHash != ""` → `Debug("deploy handed to a helper, skipping")`,
   return nil. Kein zweiter Helper, kein erneutes Schreiben.
2. Secrets, Env, `pre_deploy`, `WriteStackEnv` laufen **im Agent** weiter —
   sie starten nichts neu. `env.WriteShm` bleibt für den Fall, dass ein Hook sie
   braucht; der Plan nennt für den Helper aber `<stackDir>/.env`
   (`WriteStackEnv`-Datei), weil `/dev/shm` des Helpers ein anderes ist als das
   des Agenten. **Konsequenz:** der `rollbackStackEnv`-Restore nach einem
   fehlgeschlagenen Up entfällt im Self-Pfad — der Helper hat die Datei
   gebraucht, und der Agent erfährt das Ergebnis erst beim nächsten Start.
3. Pending persistieren: `Set(stack, alte Werte + DeployPendingHash=newHash,
   DeployPendingAt=now)` + `Save()`. Das ist der letzte ehrliche Stand vor dem
   Recreate; danach darf der Prozess jederzeit sterben.
4. Plan schreiben (`kind=deploy`, `services` = `upServices`-Ergebnis wie beim
   In-Process-Up — ggf. leer, `profiles` = `stack.Profiles`, `env_file` =
   `<stackDir>/.env`, `deploy_hash` = newHash, `grace`/`health`-Felder wie
   Phase 12) und Helper starten (`runHelper`).
5. Fehler beim Schreiben/Start: Pending zurückrollen (`DeployPendingHash=""`,
   `DeployPendingAt` zero), `Save`, `deployFailed`-Notify, Fehler zurückgeben.
   Es wurde nichts recreated → der nächste Pass darf es erneut versuchen.
6. Erfolg: `return nil`. **Kein** `state.Set` als deployed, **kein** Recovery,
   **kein** `deploy_failed` — das Ergebnis kennt nur der Helper, gebucht wird es
   beim nächsten Agent-Start (Finalisierung).

Kein Health-Gate im Deploy-Modus: der In-Process-Deploy gated auch nicht
(Parität). Ein kaputter Deploy zeigt sich wie bisher am Stack, nicht am Gate.

### 4. Helper-Image für den Deploy-Plan

Ein Deploy-Plan hat keine `targets`, also kein „das neue Image". Der Helper
muss trotzdem von *etwas* gestartet werden. Er läuft mit dem **eigenen Image
des Agenten** (dem bekanntermaßen funktionierenden Stand):

- neues Feld `Reconciler.selfImage` / `Deps.SelfImage` (optional), gefüllt aus
  `docker inspect <selfContainer> --format '{{.Config.Image}}'` beim Start,
  oder aus `DOCKOPS_SELF_IMAGE`, wenn gesetzt;
- unbekannt/leer → **harter Fehler** vor dem Start (wie bei `HelperArgs` ohne
  `--volumes-from`): ein Helper mit falschem Image würde den Deploy mit einer
  anderen Version fahren als der Agent sie hat.

### 5. Helper: Deploy-Modus

`cmd/dockops/selfupdate.go`, `runPlan` verzweigt auf `plan.Kind`:

- `images` (oder leer): unverändert (up `--no-deps` auf die Services,
  Health-Gate, Rollback, Result).
- `deploy`: Grace abwarten, dann **normaler** Up wie `compose.Up` ihn fährt
  (`-p`, `-f`, `--profile`, `--env-file`, `up -d`, Service-Liste aus dem Plan —
  leer = alle), kein `--no-deps`, **kein** Health-Gate, **kein** Rollback
  (`RollbackOverride` würde bei leeren Targets fehlschlagen). Ergebnis:
  `StatusOK` bei Erfolg, sonst neuer Status `StatusFailed` mit `Error`-Text.
  Danach Result schreiben (`Kind: "deploy"`, `DeployHash` mitführen).

Argument-Bau nicht duplizieren: die Args aus dem Plan über `internal/compose`
erzeugen (Request aus Plan-Feldern), damit Helper- und Agent-Pfad dieselbe
Konvention haben. Falls das einen Export braucht, ist ein kleiner Export
(`compose.UpArgs`) der richtige Weg — kein zweiter Satz Flags.

`Result` bekommt additiv `Kind string` (`omitempty`) — die Finalisierung muss
den Typ kennen, ohne den Plan erneut zu lesen.

Neue Statuskonstante: `StatusFailed = "failed"` (nur Deploy-Pläne; bei
`kind=images` bleibt sie ungültig). `ReadResult` akzeptiert sie.

### 6. Finalisierung

`finalizeSelfUpdate` behandelt **zuerst** ein Deploy-Pending, dann wie bisher
das Image-Pending:

| Zustand | Aktion |
|---|---|
| Result `ok` | `Set(stack, Hash=DeployPendingHash, Status=deployed, …)`, Pending räumen, `Save`, Result löschen, `recovery(deploy_failed)` |
| Result `failed` | Pending räumen, `Hash=DeployPendingHash`, `Status=failed` (→ nächster Pass retried), `Save`, `deployFailed` mit `Error`-Text |
| Result `rolled_back`/`rollback_failed` bei Deploy-Plan | wie `failed`, Text + `RollbackFailedTag` (darf nicht vorkommen — wenn doch, ist es ein Bug und soll laut sein) |
| kein Result, `now - DeployPendingAt > grace + healthtimeout + 120s` | wie `failed`, Text „helper left no result for the deploy" — sonst bliebe das Pending ewig stehen und der Stack würde nie wieder deployt |
| kein Result, noch innerhalb des Timeouts | pending lassen, Debug-Log |

Jeder Fehler wird geloggt und geschluckt (Buchhaltung darf den Pass nicht
kosten) — wie in Phase 12.

### 7. Nicht im Scope (dokumentierte Grenzen)

- **Prune des Self-Stacks** (`compose down`) bleibt in-process. Der Agent stirbt
  dabei zwar, aber der Stack verschwindet sowieso; das State-Entry bleibt und
  der nächste Pass wiederholt den Down. In README als Grenze notieren.
- `DOCKOPS_SELF_UPDATE=off` schaltet den Intercept auch für Deploys ab
  (in-process wie vor Phase 12) — Debug-Schalter, nicht Betriebsmodus.
- Kein paralleler Pass, ein Self-Stack pro Host (Grenzen aus Phase 12 gelten).

## Testfälle

`internal/selfupdate`:

- `Validate`: `kind=deploy` + `targets` gesetzt → Fehler; `kind=deploy` +
  `services` leer + `deploy_hash` gesetzt → ok; `kind=images` + `services` leer
  → Fehler; unbekannter `kind` → Fehler.
- `ReadPlan`: Plan **ohne** `kind` (v1-Datei) liest als images; Plan mit
  `kind=deploy` liest durch.
- `ReadResult`: `failed` akzeptiert, unbekannter Status weiter abgelehnt.

`internal/state`: Pending-Felder roundtrip; alte Datei ohne Felder lädt
unverändert.

`internal/reconciler`:

- Self-Stack mit geändertem Hash → **kein** `compose.Up`, stattdessen
  `runHelper` mit erwarteten Args; Plan-Datei enthält `kind=deploy`,
  `deploy_hash`, `profiles`, `env_file=<stackDir>/.env`, Services wie
  `upServices`.
- Pending gesetzt → zweiter Pass startet keinen zweiten Helper und ruft kein
  `compose.Up`.
- Helper-Start schlägt fehl → Pending geräumt, `deployFailed`, Fehler zurück.
- Nicht-Self-Stack: unverändert in-process.
- Finalisierung `ok` → Hash deployed, Pending leer, Result weg.
- Finalisierung `failed` → Hash = pending, Status failed, `deployFailed`.
- Finalisierung ohne Result innerhalb/außerhalb des Timeouts.
- Image-Apply-Pfad (Phase 12) bleibt grün — die bestehenden `phase12_test.go`
  dürfen sich nicht ändern müssen.

`cmd/dockops`: Deploy-Plan mit gestubbten Runnern → ein `compose … up`-Aufruf,
kein `--no-deps`, kein rollback-Override, Result `ok`; Fehlerfall → Result
`failed` mit Text.

## Definition of Done

`go build ./...`, `go vet ./...`, `go test ./... -count=1` — alle Pakete `ok`,
Testanzahl ≥ Baseline (vor der Phase zählen!). `gofmt -w` auf allen geänderten
Dateien. README-Abschnitt „Self-Update": Deploy-Pfad, Pending-Felder,
Grenzen (Prune), `DOCKOPS_SELF_IMAGE`. CLAUDE.md: neue Paket-/Feld-Verweise.
