FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/webhook ./cmd/external-dns-webhook-dnscale

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/webhook /webhook
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/webhook"]
