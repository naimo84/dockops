# dockops-fleet image: the read-only aggregator. Static Go binary, distroless
# static runtime — no shell, no package manager, no CA bundle needed beyond
# what distroless ships (the agents are reached over HTTPS with public or
# internal CA roots baked in there).
#
# Build context is the repository root:
#   docker build -f cmd/dockops-fleet/Dockerfile -t dockops-fleet .

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dockops-fleet ./cmd/dockops-fleet

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dockops-fleet /usr/local/bin/dockops-fleet
# FLEET_HOSTS / FLEET_STATUS_TOKENS / LISTEN come from the rendered env.
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/dockops-fleet"]
