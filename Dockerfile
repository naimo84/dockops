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
#
# Runtime hardening (deterministic, no docker-slim probing — slim 1.40.11
# pins the Docker API at 1.24 and dies against Docker >= 29 daemons, and a
# probe would never cover every exec'd CLI path anyway): keep exactly what
# internal/git, internal/compose and internal/hooks reach — the git binary
# (plus its git-core helpers: local-path clones spawn git-upload-pack, HTTPS
# remotes need git-remote-https), the docker CLI with the compose plugin,
# ssh + keygen/keyscan for git-over-SSH, busybox sh for the hooks, and the
# CA roots. Everything else with a shell/package surface goes: apk (no
# package manager in the runtime image), scp/sftp/ssh-agent (git only ever
# needs the ssh transport), git-shell, and man/doc trees.
RUN apk add --no-cache docker-cli docker-cli-compose git openssh-client ca-certificates \
	&& rm -rf /usr/share/man /usr/share/doc /usr/share/apk /usr/libexec/git-core/git-gui* \
	&& rm -f /sbin/apk /sbin/apk.static \
	         /usr/bin/scp /usr/bin/sftp /usr/bin/ssh-copy-id /usr/bin/ssh-agent /usr/bin/ssh-add \
	         /usr/bin/ssh-pkcs11-helper /usr/bin/findssl.sh /usr/bin/git-shell \
	         /usr/bin/scanelf /usr/bin/ldd
COPY --from=build /out/dockops /usr/local/bin/dockops
# /var/lib/dockops holds state.json (flag -state) and, with the default
# global.work_dir, the host and remote checkouts below /var/lib/dockops/repos.
VOLUME /var/lib/dockops
ENTRYPOINT ["/usr/local/bin/dockops"]
