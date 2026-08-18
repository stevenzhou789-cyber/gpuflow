# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM node:22-alpine AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN mkdir -p ../internal/webui && npm run build

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS go-build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
COPY --from=web-build /src/internal/webui/dist ./internal/webui/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/gpuflow ./cmd/gpuflow

FROM alpine:3.21
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=go-build /out/gpuflow /usr/local/bin/gpuflow
LABEL org.opencontainers.image.title="GPUFlow" \
      org.opencontainers.image.description="Lightweight BYOC GPU batch scheduler and agent" \
      org.opencontainers.image.licenses="MIT"
ENTRYPOINT ["gpuflow"]
CMD ["server"]
