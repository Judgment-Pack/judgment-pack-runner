# Build from the Runner repository root, not this subdirectory.
FROM golang:1.26.5-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/cloudgoogle ./internal/cloudgoogle
COPY cmd/jpack-google-relay ./cmd/jpack-google-relay
RUN CGO_ENABLED=0 go build -trimpath -o /relay ./cmd/jpack-google-relay
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /relay /relay
ENTRYPOINT ["/relay"]
