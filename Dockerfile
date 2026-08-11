# syntax=docker/dockerfile:1
FROM golang:1.24.2-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/embedded-cluster-dr ./cmd/embedded-cluster-dr

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/embedded-cluster-dr /embedded-cluster-dr
USER 65532:65532
ENTRYPOINT ["/embedded-cluster-dr"]

