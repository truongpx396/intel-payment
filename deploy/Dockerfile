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
# Builds every main package under cmd/ into /out, so a new binary needs no Dockerfile edit.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/ ./cmd/...

# --- runtime -----------------------------------------------------------------
# Distroless, non-root. Nothing in this image can run a shell, which matters for a
# container that holds provider credentials and can mint credits.
FROM gcr.io/distroless/static-debian12:nonroot
# The migrations are embedded in the binaries (migrations/embed.go): there is no directory to copy.
COPY --from=build /out/ /usr/local/bin/
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/paymentd"]
