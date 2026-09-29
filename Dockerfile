FROM --platform=$BUILDPLATFORM golang:1.26.0-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS=linux TARGETARCH=arm64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /plugin ./cmd/forgejo-runner-kubernetes
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/sjones512/forgejo-runner-kubernetes" \
      org.opencontainers.image.description="Experimental Forgejo Runner Kubernetes execution plugin"
COPY --from=build /plugin /plugin
ENTRYPOINT ["/plugin"]
