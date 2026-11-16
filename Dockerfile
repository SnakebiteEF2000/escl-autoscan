# Build stage: cross-compiles on the build platform for each target platform.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:ec47ea3725fea0cc88f6de94919aef19f40f12fc1ec0e2d1b3426dbf15ecca04 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/escl-autoscan ./cmd/escl-autoscan

# Scanner simulator for local tests (compose.yaml). The released image is the
# last stage and does not contain it.
FROM build AS simulator-build
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -o /out/simulator ./internal/escltest/simulator

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS simulator
COPY --from=simulator-build /out/simulator /simulator
USER 65532:65532
EXPOSE 8443 8081
ENTRYPOINT ["/simulator"]

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/escl-autoscan /escl-autoscan
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/escl-autoscan"]
