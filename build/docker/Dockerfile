# Multi-stage Dockerfile that builds every ObserveX service from the
# same root module. The target service is selected via the SERVICE
# build arg:
#
#   docker build -f build/docker/Dockerfile \
#       --build-arg SERVICE=ingest-gateway .
#
# The runtime image is distroless/static — no shell, no package
# manager, no libc. Image size is in the 10–20 MiB range per binary.

# ─── builder ───────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

ARG SERVICE
RUN test -n "${SERVICE}" || (echo "ERROR: SERVICE build arg is required" && exit 1)

WORKDIR /src
RUN apk add --no-cache git ca-certificates

# Cache dependencies before copying source so a code change doesn't
# bust the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Strip debug info to keep binaries small; -trimpath for build
# reproducibility; CGO disabled for distroless/static compatibility.
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" \
    -o /out/${SERVICE} ./services/${SERVICE}/cmd

# ─── runtime ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

ARG SERVICE
ENV SERVICE_NAME=${SERVICE}

COPY --from=builder /out/${SERVICE} /app/server

# Ports are documented per-service; expose all the well-knowns. The
# runtime only binds to the ones a given service uses.
EXPOSE 4317 4318 5000 7000 7100 7200 7300 7700 7800 8080 8125 9090

USER nonroot:nonroot
ENTRYPOINT ["/app/server"]
