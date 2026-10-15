# Phase 7: WIP-Auto-Stash vor Repo-Reset

Projekt: `/home/hermes/projects/dockops`, Go 1.25.4 (`export PATH=$HOME/.local/go/bin:$PATH`).

## Problem
Nutzer arbeiten manchmal DIREKT auf den Hosts im Checkout des Host-Repos (z.B. `/var/lib/dockops/repos/host`).
Der Reconciler macht bei vorhandenem Repo `fetch + reset --hard + clean -fd` — lokale Änderungen
(modifizierte UND untracked Dateien) werden kompromisslos vernichtet.

## Anforderung: Auto-WIP-Stash in `internal/git` (EnsureRepo, Update-Pfad)
1. VOR `fetch/reset --hard/clean`: `git status --porcelain` prüfen.
2. Ist der Worktree dirty (auch untracked): `git stash push --include-untracked -m "dockops-wip: <RFC3339-Timestamp>"`.
3. Nach dem Stash: INFO-Log (mit Stack/repo-Kontext) + Hinweis auf Wiederherstellung (`git stash list` / `git stash pop`).
4. Fehler beim Stash (z.B. keine git-identity nötig — stash braucht normalerweise keine) dürfen den Reconcile NICHT abbrechen: dann WARN-Log + weiter wie bisher (reset läuft, Verhalten wie heute).
5. Clean Worktree: kein Stash, kein Extra-Log (oder debug).
6. Clone-Pfad (frisches Repo): kein Stash nötig.
7. Stash NICHT automatisch anwenden/poppen — bewusst: Änderungen bleiben im Stash-Regal, Konflikte dürfen Deploys nie blockieren. In README + CLAUDE.md dokumentieren (kurzer Abschnitt "WIP-Auto-Stash").

## Tests (internal/git)
- Dirty Worktree (modifiziertes tracked File) → nach EnsureRepo ist das File im Stash (`git stash list` zeigt 1 Eintrag mit passender Message), Worktree clean, Reset auf origin/main durch.
- Untracked File → ebenfalls gestasht (Stash-Entry enthält es: `git stash show --include-untracked` bzw. nach `stash pop` vorhanden — in Test per `git stash pop` verifizieren).
- Clean Worktree → `git stash list` bleibt leer.
- Stash-Failure-Simulation (z.B. git so patchen, dass stash push exit != 0 via Script-Override im PATH, wie die bestehenden Tests es machen) → EnsureRepo liefert trotzdem kein Error, Reset läuft.

## Definition of Done
- `go build ./...`, `go vet ./...`, `go test ./... -count=1` — alle grün, keine Regressionen (aktuell 199 Tests).
- README.md + CLAUDE.md Abschnitt ergänzt.
- Commit-Message: "Phase 7: WIP auto-stash before repo reset (protect host-side edits)"
