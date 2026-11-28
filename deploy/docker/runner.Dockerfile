# Build stage: official Go image pinned to an exact patch release.
FROM golang:1.26.11 AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src

# Dependencies first so this layer is cached until go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/rcss-labs/yamdrake/internal/version.Version=${VERSION} -X github.com/rcss-labs/yamdrake/internal/version.Commit=${COMMIT}" \
    -o /out/yumdrake-runner ./cmd/runner
# The artifact directory is created here because the final image has no shell.
RUN mkdir -p /out/artifacts

# Final stage: distroless static, nonroot (tag gcr.io/distroless/static-debian12:nonroot).
FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/yumdrake-runner /yumdrake-runner
# Owned by the service user so a named volume mounted here inherits the ownership.
COPY --from=build --chown=65532:65532 /out/artifacts /var/lib/yumdrake/artifacts
USER 65532:65532
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=3 CMD ["/yumdrake-runner","healthcheck"]
ENTRYPOINT ["/yumdrake-runner"]
