# syntax=docker/dockerfile:1
# GO_VERSION has no default: .go-version is the single source of truth for
# the toolchain version (see CLAUDE.md), so an unset build-arg here must
# fail the build (empty tag -> invalid `FROM golang:-alpine` reference)
# rather than silently drifting from it. `make image` supplies it.
ARG GO_VERSION
ARG HELM_VERSION=3.19.0
ARG KUBECTL_VERSION=1.34.1

FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# internal/web/embed.go embeds internal/web/dist: go:embed patterns cannot
# cross "../", so the SPA build cannot live at web/dist (where Vite writes
# it) and be embedded directly. Reproduce what `make web` does on the host:
# build the SPA, then place its output where the embed directive expects it.
RUN rm -rf internal/web/dist
COPY --from=web /src/web/dist ./internal/web/dist
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X github.com/mchmarny/aicrme/internal/version.Version=${VERSION} -X github.com/mchmarny/aicrme/internal/version.Commit=${COMMIT}" \
      -o /out/aicrme ./cmd/aicrme

# The console shells out to the bundle's deploy.sh, which needs bash, helm,
# kubectl, and jq (the webhook preflight degrades without jq).
FROM alpine:3.22
ARG HELM_VERSION
ARG KUBECTL_VERSION
ARG TARGETARCH
RUN apk add --no-cache bash ca-certificates curl jq tar && \
    curl -fsSL "https://get.helm.sh/helm-v${HELM_VERSION}-linux-${TARGETARCH}.tar.gz" | tar -xz -C /tmp && \
    mv /tmp/linux-${TARGETARCH}/helm /usr/local/bin/helm && \
    curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" && \
    chmod +x /usr/local/bin/kubectl && \
    rm -rf /tmp/linux-${TARGETARCH} && \
    apk del curl tar && \
    adduser -D -u 10001 aicrme
COPY --from=build /out/aicrme /usr/local/bin/aicrme
USER 10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/aicrme"]
