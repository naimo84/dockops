# dockops Log-API — Design

Status: Entwurf zur Review
Datum: 2026-10-25
Ersetzt: Dozzle (zentraler Stack + `dozzle-agent` auf jedem Host)

## 1. Ziel

dockops bekommt eine **Maschinen-Log-API**, die den Dozzle-MCP ersetzt: Container-Inventar,
Log-Snapshot mit serverseitiger Suche und Live-Tail. Kein UI, kein Container-Action-Endpoint,
keine eigene Persistenz.

Erfolgskriterium: für jeden Fleet-Host lassen sich Container-Liste, letzte N Log-Zeilen,
ein Zeitfenster mit Suchbegriff und ein Live-Stream ausschließlich per HTTPS + Bearer abrufen —
Dozzle wird danach abgeschaltet, ohne dass eine Frage offen bleibt, die vorher Dozzle beantwortet hat.

### Ausgangslage (verifiziert 2026-10-25)

- dockops-HTTP-Surface: `POST /webhook`, `GET /healthz`, `GET /status`. Kein `docker logs` im Code;
  `compose.Executor` kann nur `up/pull/down/ps/images`.
- VictoriaLogs enthält **nur** `*/systemd_journal` + `opnsense/syslog` — Alloy hat keinen
  `loki.source.docker`. Container-stdout ist dort nicht vorhanden. Dozzle ist heute die einzige
  Quelle für Container-Logs.
- Ingress + DNS für dockops existieren bereits: Traefik-Router `dockops-webhook` in
  `docker/infrastructure/dockops/compose.yaml` auf `Host(${DOCKOPS_FQDN}) && (Path('/webhook') || Path('/healthz'))`,
  LE-dns, `constraint-label: public`; A-Records `dockops.<host>.naimo84.dev` in
  `IaC/playbooks` → `playbooks/networking/tdns-records/vars/records.yaml` für alle sieben Hosts
  (LAN-IPs bzw. Traefik-Tailscale-Sidecar-IPs, also nicht öffentlich routbar).

## 2. Nicht-Ziele

- Kein Web-UI, keine Container-Aktionen (`restart`/`stop`), keine Shell.
- Keine Metriken/Stats (deckt Beszel ab), kein Container-Event-Stream.
- Keine eigene Log-Persistenz, kein Index, keine Retention-Verwaltung: Historie ist die, die der
  Docker-Log-Treiber des Containers führt.
- Keine host-übergreifende Aggregation im Server. Der Client (Agent) fächert über die
  Host-Endpoints auf und mischt selbst; ein `logs.peers`-Modus wäre ein separates Feature.
- Keine Änderung an `/status`-Semantik oder Auth-Verhalten von `/webhook`.

## 3. API

Alle Endpoints: `GET`, Auth per `Authorization: Bearer <DOCKOPS_LOGS_TOKEN>`, Antwort `404` für
unbekannte Pfade, `405` bei falscher Methode.

### 3.1 `GET /logs/containers`

Inventar aller Container des Hosts — Quelle `docker ps -a --format '{{json .}}'`.

```json
[{"container":"n8n-1","project":"n8n","service":"n8n","image":"docker.n8n.io/n8nio/n8n:1.9",
  "state":"running","health":"healthy","created":"2026-10-20T09:12:00Z"}]
```

- `project`/`service` leer, wenn der Container kein Compose-Container ist (Standalone, Sidecars).
- Sortierung: `project`, dann `service`, dann `container`; Container ohne Projekt zuletzt.
- `health` leer, wenn kein Healthcheck definiert ist.

### 3.2 `GET /logs` — Snapshot

Query-Parameter:

| Param | Bedeutung | Default | Grenze |
|---|---|---|---|
| `container` | genau ein Containername | — | Regex-Allowlist, s. §5 |
| `project` | alle Container eines Compose-Projekts | — | Regex-Allowlist |
| `since` | Zeitfenster-Start: Go-Duration (`90s`, `2h30m`, `7d`) oder RFC3339 | `1h` | max. `168h` |
| `tail` | Anzahl Zeilen vom Ende | `200` | 1..5000 |
| `grep` | Zeilenfilter | — | max. 256 Zeichen |
| `regex` | `1` = `grep` als RE2 statt Literal | `0` | — |
| `format` | `text` (roh) oder `json` (NDJSON) | `text` | — |

`container` und `project` sind **gegenseitig ausschließend**; keiner von beiden → `400`.

Antwort `text/plain`: `<RFC3339-Timestamp> <Zeile>`, bei `project` mit dem von compose gesetzten
Service-Präfix (`n8n-1  | …`), unverändert.

Antwort `application/x-ndjson` bei `format=json`, eine Zeile je Log-Zeile:

```json
{"t":"2026-10-25T09:12:03.123456789Z","c":"n8n-1","m":"Editor is now accessible"}
```

Header: `X-Dockops-Lines` (ausgelieferte Zeilen), `X-Dockops-Truncated: true`, wenn das Byte-Limit
gegriffen hat (letzte Zeile ist dann `… [dockops: truncated]`).

Fehler: `404` unbekannter Container/Projekt (erkannt an der Docker-CLI-Fehlermeldung
`No such container|no such project`), `502` sonstiger Docker-Fehler mit dessen stderr als Body.

### 3.3 `GET /logs/stream` — Live-Tail (SSE)

Dieselben Parameter, zusätzlich:

- `Content-Type: text/event-stream`, Events: `log` (ein Log-Event), `ping` (alle 15 s), `end`.
- `log`-Payload bei `format=json` ist das NDJSON-Objekt aus §3.2, sonst die Rohzeile (`data:` mit
  mehrzeiligen Einträgen als mehrere `data:`-Felder).
- `end` mit `{"reason":"limit"|"since_exhausted"|"docker_exit"|"server_shutdown"}`.
- Server-seitige Grenzen: max. 15 min Streamdauer, max. 10 MiB, max. 8 gleichzeitige Streams
  (`429` darüber). Nach Limit: `end`-Event und normaler Abschluss, kein harter Abbruch.
- Client-Disconnect (`r.Context().Done()`) beendet den `docker logs --follow`-Subprozess
  (SIGKILL nach 5 s Grace) — kein verwaister Prozess, kein Zombie-`docker`-Prozess im Container.
- `tail` wirkt hier nur als Startfenster (Standard `0` = nur neue Zeilen ab Stream-Start).

### 3.4 Authentifizierung

- `DOCKOPS_LOGS_TOKEN` **ist Pflicht**: fehlt die Variable oder ist sie leer, antwortet jeder
  `/logs*`-Request mit `503` (`logs endpoint disabled: DOCKOPS_LOGS_TOKEN unset`). Kein
  Fail-open — anders als `/status`, weil in Logs Secrets stehen (dockops hat selbst schon einen
  Gitea-Token im Klartext geloggt).
- Vergleich constant-time (`crypto/subtle`), identisches Muster wie `Deps.Token` bei `/status`.
- Falscher/fehlender Token → `401`, WARN-Log mit Remote-Addr (ohne Header-Werte).

## 4. Architektur

Drei Schichten, in derselben Aufteilung wie bisher (Docker-CLI unten, HTTP oben):

```
internal/compose   — docker-CLI-Aufrufe: PsContainers, Logs (Follow/nicht)
internal/logs      — Query-Parsing, Validierung, Zeilenfilter, Limits, Encoding
internal/webhook   — Handler, Auth, SSE-Plumbing, Routing im bestehenden ServeHTTP-switch
cmd/dockops        — Flag/Env DOCKOPS_LOGS_TOKEN, Verdrahtung der Deps
```

Kein Docker-SDK: `compose.Executor` besitzt bereits Binary-Pfad, Logger und `exec`-Kapselung;
neue Aufrufe nutzen dieselbe `output`/`run`-Infrastruktur mit `exec.CommandContext`.

### 4.1 Kommandobau

| Fall | Kommando |
|---|---|
| Container, Snapshot | `docker logs --timestamps --since <RFC3339> --tail <n> <name>` |
| Container, Follow | `docker logs --timestamps --follow --since <RFC3339> --tail <n> <name>` |
| Projekt, Snapshot | `docker compose -p <proj> logs --timestamps --no-color --since <RFC3339> --tail <n>` |
| Projekt, Follow | wie oben plus `--follow` |
| Inventar | `docker ps -a --format {{json .}}` |

`compose logs` merged die Container eines Projekts selbst und präfixt `<service>-<n>  | ` — kein
eigenes Interleaving, keine N parallelen Subprozesse. `grep` wird **nicht** an die CLI delegiert,
sondern in `internal/logs` zeilenweise angewandt (identisch für beide Fälle, RE2 wie Go-`regexp`).

### 4.2 Interfaces

```go
// internal/compose
type LogsOptions struct {
    Container string    // genau eines von beiden
    Project   string
    Since     time.Time
    Tail      int       // 0 = ohne --tail
    Follow    bool
}

type ContainerInfo struct {
    Name    string `json:"container"`
    Project string `json:"project"`
    Service string `json:"service"`
    Image   string `json:"image"`
    State   string `json:"state"`
    Health  string `json:"health"`
    Created string `json:"created"`
}

func (e *Executor) Logs(ctx context.Context, opts LogsOptions, w io.Writer) error
func (e *Executor) PsContainers(ctx context.Context) ([]ContainerInfo, error)
```

```go
// internal/logs
type Format int // FormatText | FormatJSON

type Options struct {
    Kind   Kind   // KindContainer | KindProject
    Name   string
    Since  time.Time
    Tail   int
    Grep   string
    Regex  bool
    Format Format
    Follow bool
}

// ParseQuery validiert rohe Query-Parameter; jeder Fehler ist ein 400 mit Begründung.
func ParseQuery(q url.Values, now time.Time) (Options, error)

// Source ist die Injektionsstelle für Tests (echt: *compose.Executor).
type Source interface {
    Logs(ctx context.Context, opts compose.LogsOptions, w io.Writer) error
    PsContainers(ctx context.Context) ([]compose.ContainerInfo, error)
}

// Copy liest die Quelle, filtert/limitiert und schreibt nach w.
// Rückgabe: ausgelieferte Zeilen, truncated-Flag.
func Copy(ctx context.Context, src Source, opts Options, w io.Writer) (lines int, truncated bool, err error)

// Event ist ein SSE-Ereignis (Phase B).
type Event struct {
    Name string // "log" | "ping" | "end"
    Data []byte
}

// Stream liest die Quelle im Follow-Modus und ruft emit pro Ereignis auf.
// emit blockiert (Backpressure = Client, kein Puffer); ein emit-Fehler bricht ab.
// Die Byte-/Dauer-Limits gelten hier ebenfalls; bei Erreichen kommt
// emit({Name:"end", Data:`{"reason":"limit"}`}) vor dem Return.
func Stream(ctx context.Context, src Source, opts Options, emit func(Event) error) error
```

Grenzen als Konstanten in `internal/logs` (nicht konfigurierbar): `MaxTail=5000`, `MaxSince=168h`,
`MaxBytes=2<<20`, `MaxStreamBytes=10<<20`, `MaxStreamDuration=15*time.Minute`,
`MaxConcurrentStreams=8`, `MaxGrepLen=256`. Ein konfigurierbarer Satz Limits wäre erst dann
sinnvoll, wenn es unterschiedliche Aufrufer mit unterschiedlichen Bedürfnissen gibt — gibt es nicht.

### 4.3 Verdrahtung

`webhook.Deps` bekommt ein Feld `Logs LogsSource` (nil → `/logs*` antwortet `503`) plus
`LogsToken string`. Routing im bestehenden `ServeHTTP`-switch:

```
case "/logs", "/logs/containers", "/logs/stream":   // GET, Token-Gate, dann Handler
```

`cmd/dockops` liest `DOCKOPS_LOGS_TOKEN` (leer = Feature aus, WARN beim Start) und reicht den
`*compose.Executor` als `Logs` durch.

## 5. Sicherheit

- **Token-Gate fail-closed** (§3.4), constant-time, kein „leer = offen".
- **Kein Flag-Injection**: `container` und `project` müssen `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`
  erfüllen. Ohne diese Prüfung schmuggelt ein Name, der mit `-` beginnt, Flags in `docker logs`.
- `since`/`tail`/`regex` werden strikt geparst; Parse-Fehler = `400`, niemals „interpretiere
  irgendwie".
- Keine Query-Strings ins Log (grep-Muster können Pfade oder Tokens enthalten) — geloggt wird
  Pfad + Parameter-**Namen**, nicht Werte.
- Antwortgrößen gedeckelt (§3.2/§3.3), gleichzeitige Streams begrenzt: ein Crashloop-Container
  darf weder den Agent-Context noch den dockops-Container fluten.
- `/status` bleibt loopback-only; nur `/logs*`, `/webhook`, `/healthz` gehen über den Ingress.

### 5.1 Exposure — bereits vorhanden, eine Zeile Änderung

In `docker/infrastructure/dockops/compose.yaml` die bestehende Router-Rule erweitern:

```
Host(`${DOCKOPS_FQDN}`) && (Path(`/webhook`) || Path(`/healthz`) || PathPrefix(`/logs`))
```

Kein neuer Router, kein DNS-Record (existiert für alle sieben Hosts), kein neuer Cert-Pfad.
Der `DOCKOPS_LOGS_TOKEN` wird je Host über das `vault.<host>.dockops.env`-Template aus dem
Vault-KV `docker/shared/dockops#LOGS_TOKEN` referenziert (ein Fleet-Token, nicht sieben
verschiedene — der Agent hält genau ein Credential).

Kein Authentik: Der Token ist die Authentifizierung, das Ziel ist strukturell intern (A-Records auf
`10.0.0.x` bzw. Tailscale-Sidecar-`100.x`, kein öffentliches DNS). Forward-Auth würde den
Maschinen-Zugriff nur in einen 302-Login zwingen und dafür einen zweiten, ungeschützten Router
erfordern.

## 6. Fehlerbehandlung

- Docker-CLI-Fehler im Snapshot: `502`, Body = stderr des Kommandos (gekürzt auf 4 KiB).
- Docker-CLI-Fehler im Stream: `end`-Event mit `reason:"docker_exit"`, HTTP-Status bleibt 200
  (Header sind längst gesendet) — der Client sieht den Grund im Event.
- Container mit Log-Treiber ohne Lese-Unterstützung (`journald`, `syslog`): Docker antwortet
  `configured logging driver does not support reading`; das wird als `422` mit genau dieser
  Meldung durchgereicht, damit der Aufrufer den Unterschied zwischen „Container weg" und
  „falscher Log-Treiber" sieht (Vorab-Inventar der Fleet-Log-Treiber gehört zum Rollout, §8).
- Server-Shutdown während eines Streams: `end{reason:"server_shutdown"}`.

## 7. Tests

Unit (kein Docker nötig, Fake-Binary wie in `internal/compose` üblich):

- `internal/logs/parse_test.go` — Matrix: fehlendes `container`+`project` → Fehler; beide gesetzt →
  Fehler; Namen mit `-`/`/`/Leerzeichen → Fehler; `tail` 0/1/5000/5001; `since` `7d`/`8d`/Müll;
  `grep` > 256 Zeichen; kaputte `regex=1`-Pattern.
- `internal/compose/logs_test.go` — Argument-Liste je Fall (Snapshot/Follow/Projekt), Kontext-Cancel
  killt den Prozess, `PsContainers`-Parsing inkl. Container ohne Compose-Label.
- `internal/logs/copy_test.go` — grep literal vs. regex, `tail`-Deckelung, Byte-Truncation +
  Truncation-Marker, NDJSON-Shape.
- `internal/webhook/logs_test.go` — 503 ohne Token, 401 falscher Token, 200 richtiger Token,
  400-Matrix, 404 unbekannter Container, SSE-Framing (`log`/`ping`/`end`), 429 beim neunten Stream,
  Disconnect bricht ab (Fake-Source zählt Cancels).

Livetest (devbox, Prozedur aus Skill `dockops-dev`):

1. Image bauen, `-once`-Run gegen Test-Host-Repo, Container mit bekanntem Output erzeugen.
2. `curl -H "Authorization: Bearer …" https://dockops.devbox.naimo84.dev/logs/containers` → Inventar.
3. Snapshot mit `since=10m&grep=<marker>` → genau die erwarteten Zeilen, `X-Dockops-Lines` plausibel.
4. `curl -N …/logs/stream?container=…` in einem Terminal, `docker kill`-Testcontainer im anderen →
   `end{reason:"docker_exit"}`; Client-Ctrl-C → auf dem Host kein verwaister `docker logs`-Prozess
   (`docker top dockops` / `ps`).
5. Negativprobe ohne Token → 401; mit `tail=99999` → 400.

Rollout-Verifikation je Host (nach Merge + Deploy): `curl https://dockops.<host>.naimo84.dev/healthz`
→ 200, dann `/logs/containers` → nicht-leer. Hosts ohne laufenden Traefik-Router (Vorbehalt `omv`)
fallen auf Loopback/Agent-Zugriff zurück und werden im Rollout-Report explizit als „kein Ingress"
geführt, nicht als Fehler.

## 8. Phasen

1. **Phase A — Log-API ohne Stream.** `internal/compose` (`PsContainers`, `Logs` ohne Follow),
   `internal/logs` (Parse/Copy inkl. Caps), `/logs` + `/logs/containers`, Auth-Gate, Tests.
2. **Phase B — SSE-Stream.** `Follow`-Pfad, SSE-Handler, Concurrency-Semaphore, Cancellation,
   `end`-Semantik, Tests.
3. **Phase C — Exposure + Rollout.** Infrastruktur-PR (Router-Rule), Vault-KV `docker/shared/dockops#LOGS_TOKEN`
   + Template-Zeile, Image-Release, Deploy + Verifikation je Host, Log-Treiber-Inventar der Fleet.
4. **Phase D — Dozzle-Abbau.** Zentraler Stack `docker/netcup2/dozzle`, `dozzle-agent` aus
   `docker/infrastructure`, die drei toten Agent-Registrierungen (`internal`, `devbox`, `omv` in
   `DOZZLE_REMOTE_AGENT`). Erst nach vollständiger Verifikation aus Phase C.
5. **Außerhalb des Repos:** ein dünner Client (Hermes-Skill/Skript), der die sieben Host-Endpoints
   auffächert, `containers|logs|stream` über Hosts merged und die Treffer nach Host gruppiert —
   das ersetzt den Dozzle-MCP als Ganzes.

## 9. Offene Punkte / Risiken

- **`omv`**: A-Record existiert, ob dort ein Traefik läuft ist ungeprüft (der n8n-Traefik-Sync kennt
  nur `internal`, `hvaler`, `netcup`, `netcup2`; Records für `bifrost`/`devbox`/`omv` sind manuell
  deklariert). Kein Blocker: Loopback-Zugriff bleibt.
- **Log-Treiber**: Container mit `journald`-Treiber sind über `docker logs` nicht lesbar. Wenn die
  Fleet davon Gebrauch macht, ist das eine dokumentierte Einschränkung des Features (und ein Grund,
  Alloy später doch um `loki.source.docker` zu erweitern) — kein Grund, die API zu verbiegen.
- **Kein UI**: Wer Logs „mal eben im Browser" lesen will, nutzt künftig `curl -N` oder den Client
  aus Phase 5. Das ist eine bewusste Entscheidung, nicht eine Lücke.
- **Dozzle-Lücke in VL bleibt**: Das Design ändert nichts an der Alloy-Config. Wenn zusätzlich
  historische Volltextsuche über Container-Logs gewünscht ist, ist `loki.source.docker` ein
  separates Feature (eigene Spec), kein Anhängsel dieser API.
