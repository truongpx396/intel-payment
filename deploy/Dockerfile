# syntax=docker/dockerfile:1
# --- build -------------------------------------------------------------------
FROM golang:1.23-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/paymentd        ./cmd/paymentd && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/payment-worker  ./cmd/payment-worker && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/payment-migrate ./cmd/payment-migrate

# --- runtime -----------------------------------------------------------------
# Distroless, non-root. Nothing in this image can run a shell, which matters for a
# container that holds provider credentials and can mint credits.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/paymentd        /usr/local/bin/paymentd
COPY --from=build /out/payment-worker  /usr/local/bin/payment-worker
COPY --from=build /out/payment-migrate /usr/local/bin/payment-migrate
COPY migrations /migrations
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/paymentd"]
