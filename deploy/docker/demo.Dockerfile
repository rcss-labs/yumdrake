# The demo helper (cmd/yumdrake-demo): a fake GitHub with sample data, and the
# seeding of a fresh Yumdrake. A development tool: it is built only by
# deploy/compose/compose.demo.yaml and is never part of the product images.

# Build stage: official Go image pinned to an exact patch release.
FROM golang:1.26.11 AS build
WORKDIR /src

# Dependencies first so this layer is cached until go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/yumdrake-demo ./cmd/yumdrake-demo

# Final stage: distroless static, nonroot (tag gcr.io/distroless/static-debian12:nonroot).
FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/yumdrake-demo /yumdrake-demo
COPY deploy/compose/demo/ /demo/
USER 65532:65532
HEALTHCHECK --interval=5s --timeout=3s --start-period=20s --retries=30 CMD ["/yumdrake-demo","healthcheck"]
ENTRYPOINT ["/yumdrake-demo"]
