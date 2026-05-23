# Build stage — cross-compile on the host platform for the target platform.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
# Cache module downloads separately from source.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /voidshell ./cmd/voidshell

# Final image — minimal static base, non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /voidshell /voidshell
ENTRYPOINT ["/voidshell"]
