# commercial-billing: server, jobs and migrate binaries in one distroless,
# non-root image (LLD §13). Base images follow the IAM services; pin them to
# digests before the first release (make pin-base-images in the IAM repos).
FROM golang:1.26.11-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS builder
WORKDIR /build

ENV GOPRIVATE="github.com/BCBP-SOLUTIONS-FZC-LLC/*"
ENV GONOSUMDB="github.com/BCBP-SOLUTIONS-FZC-LLC/*"
# hadolint ignore=DL3018
RUN apk add --no-cache git

ARG BUILD_VERSION=dev
ARG SOURCE_DATE_EPOCH=0

COPY go.mod go.sum ./
RUN --mount=type=secret,id=go_private_token \
    TOKEN=$(cat /run/secrets/go_private_token 2>/dev/null || true) && \
    if [ -z "$TOKEN" ]; then echo "ERROR: pass --secret id=go_private_token,src=<token-file>"; exit 1; fi && \
    git config --global url."https://x-access-token:${TOKEN}@github.com/".insteadOf "https://github.com/" && \
    go mod download && \
    git config --global --unset url."https://x-access-token:${TOKEN}@github.com/".insteadOf

COPY --link . .
RUN CGO_ENABLED=0 GOOS=linux SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" -o bin/commercial-billing ./cmd/server && \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" -o bin/jobs ./cmd/jobs && \
    go build -trimpath -ldflags="-s -w" -o bin/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
ARG BUILD_VERSION=dev
ENV BUILD_VERSION=${BUILD_VERSION}
LABEL org.opencontainers.image.title="commercial-billing" \
      org.opencontainers.image.source="https://github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service" \
      org.opencontainers.image.vendor="BCBP Solutions FZC LLC" \
      org.opencontainers.image.revision="${BUILD_VERSION}"
COPY --from=builder /build/bin/commercial-billing /commercial-billing
COPY --from=builder /build/bin/jobs /jobs
COPY --from=builder /build/bin/migrate /migrate
EXPOSE 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/commercial-billing"]
