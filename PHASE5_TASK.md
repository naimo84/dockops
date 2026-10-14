# Phase 5: E2E-Integrationstest, Dockerfile & README für "dockops"

Projekt: `/home/hermes/projects/dockops`, Go 1.25.4 (`export PATH=$HOME/.local/go/bin:$PATH`). Lies zuerst `CLAUDE.md` und alle `internal/*`-Packages + `cmd/dockops/main.go`. Bestehende Packages nur erweitern, nicht brechen.

## Aufgabe 1: End-to-End-Integrationstest `internal/reconciler/e2e_test.go` (oder eigenes Verzeichnis `test/e2e`)
Ein vollständiger Reconcile-Durchlauf mit echten Dateisystem-Repos, aber Fake-Binaries:

- Baue mit `t.TempDir()`:
  - Ein "Bare"-Host-Repo (`git init --bare`), das per `git clone`+Commit eine Root-`stacks.yaml` + lokale `stack.yaml`s enthält.
  - Ein "Bare"-Shared-Remote-Repo mit einem Import-Stack (inkl. services-Teilmenge im Import).
  - Fake-`docker`-Binary (Shell-Skript, protokolliert Aufrufe in Datei, exit 0) und Fake-`sops` — via PATH-Override im exec.Command-Env des Reconcilers.
  - Fake-Vault via `httptest.Server` (KV v2 JSON).
- Führe `Reconcile` aus und asserte:
  1. Fake-docker wurde mit `-p <stackname>`, `-f <composefile>`, `--profile`, Service-Subset, `up -d --remove-orphans` aufgerufen (Log-Datei auslesen).
  2. State-Datei enthält Hashes für deployed Stacks.
  3. Zweiter Reconcile-Durchlauf ohne Änderung → keine `up`-Aufrufe (Hash unverändert, idempotent).
  4. Änderung im Host-Repo (neuer Commit, z.B. neue env_file-Zeile oder geänderte compose) → `up` wird wieder aufgerufen.
  5. pre_deploy-Fehler (Hook `exit 1`) → Stack wird nicht deployed (kein up-Aufruf), Fehler in Reconcile-Rückgabe.
  6. Prune: Stack aus Config entfernen (neuer Commit), prune_removed_stacks=true → `down --remove-orphans` wird aufgerufen.
- Wenn der Reconciler echte `git`-Binary nutzt (tut er, internal/git): das ist ok — git ist auf der Testmaschine vorhanden.

## Aufgabe 2: Dockerfile (Multi-Stage, distroless-artig schlank)
```dockerfile
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dockops ./cmd/dockops

FROM alpine:3.20
RUN apk add --no-cache docker-cli docker-cli-compose openssh-client ca-certificates
COPY --from=build /out/dockops /usr/local/bin/dockops
VOLUME /var/lib/dockops
ENTRYPOINT ["/usr/local/bin/dockops"]
```
- Optimiere: `RUN --mount=type=cache` nur wenn es mit Docker-Buildx klar funktioniert — sonst einfach halten.
- Eine `compose.yaml` für dockops selbst: dockops-Container mit `/var/run/docker.sock` gemountet, SSH-Key-Mount (`~/.ssh/id_ed25519:/root/.ssh/id_ed25519:ro`), Env-Vars `DOCKOPS_REPO_URL`, `DOCKOPS_BRANCH`, `DOCKOPS_WEBHOOK_SECRET`, example `stacks.yaml`-Repo.
- ACHTUNG: Nicht den Container bauen (kein Docker auf dieser Maschine) — nur Dateien schreiben. Aber `docker build`-Korrektheit nach bestem Wissen prüfen (Syntax, Reihenfolge).

## Aufgabe 3: README.md
- Was ist dockops (2-3 Sätze), Architektur-Übersicht (Host-Repo + Shared-Remotes + Imports, diagramm als ASCII oder mermaid).
- Vollständiges Root-`stacks.yaml`-Beispiel (global, include, vault, remotes, imports mit beiden Varianten: kompletter Stack-Import und Container-Subset-Import).
- Beispiel lokale `stack.yaml` (immich-Beispiel aus der Spezifikation, mit hooks, env_files, sops_files, vault_paths).
- Ablauf eines Reconcile-Durchlaufs (nummerierte Schritte).
- Secret-Auflösung & Priorität (Tabelle oder Liste).
- CLI-Flags + Env-Vars (Tabelle).
- Docker-Deployment (compose.yaml-Erklärung, Socket-Mount-Hinweis, SSH-Key, Vault/SOPS-Env).
- Development: `go build ./...`, `go test ./...`.

## Definition of Done
- `go build ./...` ✅ `go vet ./...` ✅ `go test ./... -count=1` ✅ — alles grün inkl. neuem E2E-Test.
- README.md, Dockerfile, compose.yaml vorhanden und konsistent mit dem implementierten Code (Flags, Pfade, Config-Felder).
