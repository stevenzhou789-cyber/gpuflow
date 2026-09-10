FROM node:22-alpine AS web-build
ENV NODE_OPTIONS=--max-old-space-size=768
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN mkdir -p ../internal/webui && npm run build

FROM golang:1.25-alpine AS verify-and-package
ENV GOMAXPROCS=2 GOMEMLIMIT=768MiB CGO_ENABLED=0
RUN apk add --no-cache zip
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/
COPY --from=web-build /src/internal/webui/dist ./internal/webui/dist
# These checks are mandatory dependencies of BOTH image and archive targets.
RUN go test -count=1 -p 2 ./... && go vet -p 2 ./...
RUN mkdir -p /out /build/linux-amd64 /build/linux-arm64 /build/windows-amd64 && \
    GOOS=linux GOARCH=amd64 go build -p 2 -trimpath -ldflags="-s -w" -o /build/linux-amd64/gpuflow ./cmd/gpuflow && \
    GOOS=linux GOARCH=arm64 go build -p 2 -trimpath -ldflags="-s -w" -o /build/linux-arm64/gpuflow ./cmd/gpuflow && \
    GOOS=windows GOARCH=amd64 go build -p 2 -trimpath -ldflags="-s -w" -o /build/windows-amd64/gpuflow.exe ./cmd/gpuflow && \
    tar -C /build/linux-amd64 -czf /out/gpuflow-development-unsigned-linux-amd64.tar.gz gpuflow && \
    tar -C /build/linux-arm64 -czf /out/gpuflow-development-unsigned-linux-arm64.tar.gz gpuflow && \
    zip -j /out/gpuflow-development-unsigned-windows-amd64.zip /build/windows-amd64/gpuflow.exe
ARG VCS_REF
RUN test -n "$VCS_REF" && \
    printf 'product=gpuflow-community\nchannel=development\nsigned=false\ncommit=%s\nrelease_approved=false\n' "$VCS_REF" > /out/BUILD-METADATA.txt && \
    cd /out && sha256sum ./*.tar.gz ./*.zip BUILD-METADATA.txt > checksums.txt

FROM scratch AS artifacts
COPY --from=verify-and-package /out/ /

FROM alpine:3.21 AS runtime
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=verify-and-package /build/linux-amd64/gpuflow /usr/local/bin/gpuflow
ARG VCS_REF
LABEL org.opencontainers.image.title="GPUFlow development (unsigned)" \
      org.opencontainers.image.description="Local CI development build; not a signed release" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.revision=$VCS_REF \
      io.gpuflow.release.channel="development" \
      io.gpuflow.release.signed="false"
ENTRYPOINT ["gpuflow"]
CMD ["server"]
