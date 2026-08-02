# Multi-stage: build with the full Go toolchain, ship almost nothing.
# Final image is ~8 MB and contains a single static binary — no shell, no libc,
# no package manager, so nothing to exploit and nothing to patch.

FROM golang:1.25-alpine AS build

WORKDIR /src

# Copy manifests first. This layer only invalidates when dependencies change,
# so `go mod download` stays cached across ordinary code edits.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# CGO_ENABLED=0 produces a statically linked binary that runs on `scratch`.
# -s -w strips the symbol table and DWARF data (~25% smaller, and you have
# structured logs rather than needing stack symbols in production).
RUN CGO_ENABLED=0 GOOS=linux go build \
	-trimpath \
	-ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
	-o /out/server ./cmd/server

# --- runtime ---

FROM scratch

# The binary makes no outbound TLS calls today, but this costs 200 KB and saves
# a confusing debugging session the first time it needs to call an API.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=build /out/server /server

# Non-root. `scratch` has no /etc/passwd, so this is a raw UID — which is fine,
# the kernel only cares about the number.
USER 65534:65534

EXPOSE 8080

ENTRYPOINT ["/server"]
