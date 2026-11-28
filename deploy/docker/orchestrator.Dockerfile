# Web stage: builds the React application from the lockfile.
FROM node:24.22.3-alpine3.24 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Build stage: official Go image pinned to an exact patch release.
FROM golang:1.26.11 AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src

# Dependencies first so this layer is cached until go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Embedded by internal/webui; replaces whatever the context held.
COPY --from=web /web/dist/ ./internal/webui/dist/
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/rcss-labs/yamdrake/internal/version.Version=${VERSION} -X github.com/rcss-labs/yamdrake/internal/version.Commit=${COMMIT}" \
    -o /out/yumdrake-orchestrator ./cmd/orchestrator
# The log archive directory is created here because the final image has no shell.
RUN mkdir -p /out/log-archive

# Final stage: distroless static, nonroot (tag gcr.io/distroless/static-debian12:nonroot).
FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/yumdrake-orchestrator /yumdrake-orchestrator
# Owned by the service user so a named volume mounted here inherits the ownership.
COPY --from=build --chown=65532:65532 /out/log-archive /var/lib/yumdrake/log-archive
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=3 CMD ["/yumdrake-orchestrator","healthcheck"]
ENTRYPOINT ["/yumdrake-orchestrator"]
