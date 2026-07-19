# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build

ARG TARGETOS
ARG TARGETARCH
ARG MAIN_PACKAGE=./cmd/orka-gateway-telegram

WORKDIR /src
RUN apk add --no-cache ca-certificates

COPY go.* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags='-s -w' \
      -o /out/orka-gateway-telegram "${MAIN_PACKAGE}"

FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b

ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=unknown

LABEL org.opencontainers.image.title="orka-gateway-telegram" \
      org.opencontainers.image.description="Out-of-tree Telegram adapter for the Orka generic gateway protocol" \
      org.opencontainers.image.source="https://github.com/sozercan/orka-gateway-telegram" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.licenses="MIT"

ENV ADAPTER_VERSION=${VERSION}

COPY --from=build --chown=65532:65532 /out/orka-gateway-telegram /usr/local/bin/orka-gateway-telegram

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/orka-gateway-telegram"]
