# Build a static binary, then ship it on distroless.
FROM golang:1.26-bookworm AS build

WORKDIR /src

# No third-party dependencies, so there is no go.sum and nothing to download.
COPY go.mod ./

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/exporter .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/exporter /exporter

ENV PORT=3000
EXPOSE 3000

# The image has no shell or curl; the binary probes itself.
HEALTHCHECK --interval=1m --timeout=5s --start-period=30s CMD ["/exporter", "healthcheck"]

ENTRYPOINT ["/exporter"]
