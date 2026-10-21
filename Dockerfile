# Build a static binary, then ship it on distroless. Base images are pinned by
# digest; Dependabot bumps them.
FROM golang:1.26-bookworm@sha256:fe3da92a5cb2881e4aa75e625cb0b7c50571632fb4cf340cbe53e6ccdcffdd51 AS build

WORKDIR /src

# No third-party dependencies, so there is no go.sum and nothing to download.
COPY go.mod ./

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/exporter .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/exporter /exporter

ENV PORT=3000
EXPOSE 3000

# The image has no shell or curl; the binary probes itself.
HEALTHCHECK --interval=1m --timeout=5s --start-period=30s CMD ["/exporter", "healthcheck"]

ENTRYPOINT ["/exporter"]
