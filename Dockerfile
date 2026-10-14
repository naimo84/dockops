# dockops agent image. Multi-stage: a static, stripped binary from the Go
# builder, then a slim alpine runtime carrying every CLI dockops shells out
# to — docker + compose plugin for the stacks, git for the checkouts, ssh
# for git over SSH — plus CA roots for HTTPS remotes and Vault.
#
# No RUN --mount=type=cache mounts: they need BuildKit, and plain layer
# caching (go.mod/go.sum copied before the source) already keeps rebuilds
# cheap. Keep it simple and portable.

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dockops ./cmd/dockops

FROM alpine:3.20
# git is not in the phase-5 briefing's package list but is required at
# runtime: internal/git drives the locally installed git binary, and every
# reconcile pass fails at git.New without it.
RUN apk add --no-cache docker-cli docker-cli-compose git openssh-client ca-certificates
COPY --from=build /out/dockops /usr/local/bin/dockops
# /var/lib/dockops holds state.json (flag -state) and, with the default
# global.work_dir, the host and remote checkouts below /var/lib/dockops/repos.
VOLUME /var/lib/dockops
ENTRYPOINT ["/usr/local/bin/dockops"]
