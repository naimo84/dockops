# Fleet-Oberfläche für dockops (read-only) & billiger `/status`

Datum: 2026-10-28 · Status: Entwurf · Vorgänger: Phase 14 (Ops-lesbares Stack-Env)

## Problem

Drei Dinge hängen zusammen:

1. **Kein Fleet-Blick.** `/status` ist pro Host, `127.0.0.1`-gebunden und bewusst
   nicht im Traefik-Router. Wer wissen will, ob die Fleet steht, fragt sieben
   Hosts einzeln ab — oder gar nicht.
2. **`/status` skaliert mit der Stack-Zahl.** Jeder Aufruf startet **einen
   `docker compose -p <projekt> ps --all` pro verwaltetem Stack plus ein
   `docker ps`** (`buildStackStatus` → `PsProject`, danach `PsAllProjects`),
   seriell. Gemessen auf bifrost: `compose -p beszel-agent ps` um
   `05:33:09.092`, `ps --format {{json .}}` um `05:33:09.201` — rund 110 ms für
   einen Stack inklusive Basis. Heute sind es 1–2 Stacks pro Host
   (`state saved stacks=1|2`) und damit ~175–200 ms pro Aufruf. Ziel-Zustand der
   Fleet sind ~100 Stacks: **~5–6 s pro Aufruf und 100 Prozess-Starts.** Damit
   ist weder eine Übersichtsseite noch ein Monitoring-Poll sinnvoll.
3. **`/status` antwortet ohne Token.** Ein Aufruf ohne `Authorization` liefert
   `200`. Auf Loopback egal; sobald der Endpunkt hinter Traefik steht, ist das
   ein offener Deployment-State.

Die Oberfläche ist also nicht das erste Problem. Sie ist die Rechtfertigung für
den Umbau, der ohne sie aufgeschoben würde.

## Lösung — zwei PRs, in dieser Reihenfolge

### PR 1 — `/status` aus einem einzigen Docker-Aufruf

`compose.PsContainers()` existiert bereits (`internal/compose/logs.go`): **ein**
`docker ps -a --format {{json .}}`, Projekt und Service aus den
`com.docker.compose.*`-Labels, Health aus der Status-Zeile. Genau dieselbe
Information, die `buildStackStatus` heute pro Stack einzeln erfragt.

- `webhook.ComposeLister` schrumpft auf
  `PsContainers(ctx) ([]compose.ContainerInfo, error)`. `PsProject` und
  `PsAllProjects` fliegen aus dem Interface (nicht aus `compose.Executor` — der
  Reconciler braucht `PsProject` weiter).
- `BuildStatus` ruft einmal `PsContainers`, gruppiert nach `Project` in
  `map[string][]compose.Container` (Name/State/Health/Service), und baut daraus
  `stacks` **und** `unmanaged`.
- `unmanaged` behält seine Semantik „Projekt mit **laufenden** Containern ohne
  State-Eintrag": aus der `-a`-Liste wird deshalb auf `State == "running"`
  gefiltert. Ohne diesen Filter tauchen gestoppte Fremd-Container als
  „unmanaged" auf — das wäre eine stille Verhaltensänderung.
- `Deps.SelfProject` bleibt unverändert ausgenommen.
- **Degradation ändert sich bewusst:** Ein Fehler des einen Docker-Aufrufs setzt
  *alle* Stacks auf `drift=unknown` plus `error`, statt einen Stack. Begründung:
  ein kaputter Docker-Aufruf ist hostweit, nicht stack-spezifisch; die
  Fehlerantwort bleibt HTTP 200 mit gefülltem `error`-Feld, wie heute bei
  `PsAllProjects`.
- Drift-Matrix (`ok`/`stopped`/`partial`/`unknown`), JSON-Shape, Hash-Schema und
  State-File bleiben unberührt. `-print-status` (CLI) nutzt denselben
  `BuildStatus` und erbt den Effekt automatisch.

Ergebnis: konstant ein Docker-Aufruf, ~150 ms, unabhängig von 1 oder 100 Stacks.

**Grenze, ehrlich:** `docker compose ps --all` und `docker ps -a` sind nicht
byte-identisch. Compose listet einen Stack auch dann mit Containern, wenn
Docker die Container zwar anlegt, aber nicht mehr führt (verwaiste
`config-hash`-Zustände) — dann meldet der neue Pfad `stopped` statt `partial`.
Für den Betrieb ist das die ehrlichere Aussage: was Docker nicht listet, läuft
nicht. Die Live-Abnahme prüft genau diesen Fall auf einem Host mit einem
gestoppten Stack.

### PR 2 — Aggregator und Seite

Ein Binary, eine Seite, kein Build-Schritt.

```
cmd/dockops-fleet/
├── main.go        # Flags/Env, HTTP, Merge, go:embed
├── index.html     # Markup + CSS + JS inline
├── main_test.go   # httptest gegen Fake-Hosts
└── Dockerfile     # distroless/static, nur das Binary
```

**Konfiguration** (aus `vault-env`, keine Datei, kein DB):

```
FLEET_HOSTS="hvaler=https://dockops.hvaler.naimo84.dev,netcup2=https://dockops.netcup2.naimo84.dev,…"
FLEET_STATUS_TOKENS="hvaler=…,netcup2=…,…"    # je Host eigenes Secret
LISTEN=:9090
```

`FLEET_STATUS_TOKENS` wird beim Start gegen `FLEET_HOSTS` geprüft: fehlt ein
Host im Token-Set, startet der Prozess nicht. Ein Token pro Host, nicht ein
geteiltes — ein Leak öffnet dann einen Host, nicht die Fleet.

**`GET /api/fleet`** — holt alle `<base>/status` **parallel** (5-s-Timeout,
`Authorization: Bearer`), Reihenfolge wie `FLEET_HOSTS`, Antwort:

```json
{"generated_at":"…","hosts":[
  {"name":"hvaler","url":"https://…","ok":true,"status":{…}},
  {"name":"omv","url":"https://…","ok":false,"error":"status 401: …"}]}
```

Kein Cache, kein State auf Platte: der Umbau aus PR 1 macht den Aufruf billig,
und ein Cache wäre nur eine zweite Wahrheit. Ein toter Host kostet 5 s und
blockiert die anderen nicht.

**`GET /`** — eingebettete Seite: Karte pro Host mit Drift-Ampel
(`ok`/`partial`/`stopped`/`unknown`), Stack-Liste (Container, Update-Kandidat
mit kurzem Digest, letzter Deploy relativ), ein Fehlertext statt Ampel bei
`ok=false`. Sortierung: Probleme zuerst, dann Name. Auto-Refresh 30 s, Dark,
kein Framework, keine externen Assets (kein CDN im internen Netz).

**Deployment**

- Neuer Matrix-Eintrag in `.github/workflows/build.yml`:
  `image: dockops-fleet`, eigenes Dockerfile → `ghcr.io/naimo84/dockops-fleet:latest|:sha|:semver`.
- Stack `dockops-fleet/` im `docker/infrastructure`-Repo (`compose.yaml`,
  `stack.yaml`, `vault.dockops-fleet.env`), Einsatz auf hvaler, deployt von
  dockops selbst.
- Ingress `dockops.naimo84.dev` mit Authentik-SSO über Blueprint
  (`custom-blueprints/dockops-fleet-sso.yaml`, Muster litellm), kein
  UI-Geklicke.
- Exposure der Agenten: in `infrastructure/dockops/compose.yaml` die
  Router-Rule um `Path(`/status`)` erweitern.

**Reihenfolge, zwingend**

1. Status-Token **pro Host** in Vault schreiben
   (`docker/data/hosts/<host>/dockops#DOCKOPS_STATUS_TOKEN`), Template
   `vault.<host>.dockops.env` um die Zeile erweitern.
2. Dann Compose-Merge mit Rule + Token. Beides kommt aus demselben `.env`-Render;
   wenn der Token beim ersten Deploy fehlt, steht `/status` offen.
3. Danach Aggregator deployen, dann Authentik-App.

## Nicht im Scope

- Aktionen aus der UI: Restart, Apply, Logs, Deploy-Trigger. Kein neuer
  Schreibpfad, keine zweite Meinung darüber, wann deployt wird.
- History, Timeline, Graphen, Metriken. „Letzter Deploy" ist der Zeitstempel aus
  dem State, keine Historie.
- Rollen/Rechte jenseits von Authentik-SSO; kein RBAC, kein OIDC im Aggregator.
- Cache, Datenbank, Websocket, Push.
- Gatus-Endpoint auf `/status` (nach PR 1 billig, aber eigenes Thema).
- Umbau von `PsProject` im Reconciler.

## Betroffene Stellen

| Datei | Änderung |
|---|---|
| `internal/webhook/webhook.go` | `ComposeLister` → nur `PsContainers` |
| `internal/webhook/status.go` | ein Aufruf, Gruppierung, `unmanaged`-Filter, Degradation |
| `internal/webhook/status_test.go` | `fakeCompose` auf `ContainerInfo`, neue Fälle |
| `internal/webhook/*_test.go` | Fakes, die `PsProject` implementieren |
| `cmd/dockops-fleet/*` | neu |
| `.github/workflows/build.yml` | Matrix-Eintrag |
| `dockops/compose.yaml` (infra) | Rule + `DOCKOPS_STATUS_TOKEN` |
| `dockops-fleet/*` (infra) | neu |
| `custom-blueprints/dockops-fleet-sso.yaml` | neu |

## Tests

**PR 1** (`internal/webhook/status_test.go`, `fakeCompose` mit fester
`ContainerInfo`-Liste):

1. Drei Projekte im State, Container gemischt → Drift `ok`/`partial`/`stopped`
   wie in der bestehenden Matrix.
2. Fremdprojekt mit **laufendem** Container → in `unmanaged`.
3. Fremdprojekt mit **gestopptem** Container → **nicht** in `unmanaged`
   (pinnt den `running`-Filter).
4. `SelfProject` läuft → nicht in `unmanaged`.
5. `PsContainers` liefert Fehler → alle Stacks `unknown`, `error` gesetzt, HTTP
   200, Antwort weiterhin parsebar.
6. Token-Gate: ohne Token 401, mit Token 200 (bestehende Tests bleiben).
7. `go build ./... && go vet ./... && go test ./... -count=1`.

**PR 2** (`cmd/dockops-fleet/main_test.go`, `httptest`):

1. Drei Fake-Hosts: 200 mit Status-JSON, 500, Timeout → `ok` false mit
   `error`-Text, die übrigen Hosts vollständig in der Antwort.
2. Der Bearer-Token jedes Hosts wird gesendet (Header-Assertion im Fake).
3. Reihenfolge der Antwort = Reihenfolge in `FLEET_HOSTS`.
4. Start mit fehlendem Token für einen konfigurierten Host → Startfehler.

Kein Test für `index.html` — sie enthält keine Logik außer Rendern und
Sortieren nach dem, was das JSON liefert. Bewusst, nicht vergessen.

## Live-Abnahme

1. **Vor dem Umbau** auf bifrost: `curl -s http://127.0.0.1:8180/status`
   (Wrapper liefert nur Code + Zeit) → 200 in ~0,2 s notieren.
2. **Nach dem Umbau**: gleicher Aufruf. Harter Beweis ist das Log, nicht die
   Zeit: `docker logs dockops --tail=20` darf pro `/status` nur noch **eine**
   `docker command finished`-Zeile zeigen (`ps -a --format {{json .}}`), keine
   `compose -p … ps` mehr.
3. Gestoppten Stack auf einem Test-Host herstellen → `drift` muss `stopped`
   sein, nicht `partial` und nicht verschwinden.
4. Aggregator gegen die echte Fleet: 7 Karten, ein Host absichtlich mit falschem
   Token → rote Karte mit Fehlertext, Seite rendert weiter.
5. `https://dockops.naimo84.dev` hinter Authentik: ohne Login kein Inhalt.

## Risiken

- **Exposure:** `/status` verrät Stack-Namen, Container-Zustände und
  Image-Digests. Deshalb Token pro Host, `Path(`/status`)` nur im bestehenden
  Router, kein `PathPrefix(`/`)`.
- **Ein-Aufruf-Degradation:** fällt `docker ps` aus, ist der ganze Report blind
  statt teilweise. Gewollt (hostweit), aber im Betrieb sichtbarer als vorher.
- **DNS:** `dockops.<host>.naimo84.dev`-Records existieren für die FQDN, die
  heute schon im Router stehen; ob alle sieben Hosts einen haben, ist vor PR 2
  zu prüfen (Split-DNS, tdns). Nicht verifiziert.
- **Vault muss offen sein** (`/v1/sys/seal-status`), sonst rendert kein
  `.env`-Template und der Token-Deploy scheitert still.
