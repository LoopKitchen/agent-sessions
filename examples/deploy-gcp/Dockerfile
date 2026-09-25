# Build context is the REPOSITORY ROOT, not this directory:
#
#   docker build -f examples/deploy-gcp/Dockerfile .
#
# The server imports internal/event and internal/health, which the client also
# uses, so the module has to be present whole. Building from this directory
# would silently exclude them.

FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies are copied and downloaded before the source so an edit to a
# handler does not invalidate the module cache layer. go.sum is optional
# because the client is dependency-free and the file may not exist yet.
COPY go.mod ./
COPY go.su[m] ./
RUN go mod download

COPY . .

# MAIN_PKG is an argument rather than a constant because the server entrypoint
# is owned by a different part of the tree and may move. Overriding it here is
# cheaper than rewriting this file when it does.
ARG MAIN_PKG=./server/cmd/loop-sessions-server
ARG VERSION=dev
ARG BUILD_DATE=

# CGO off produces a static binary, which is the only kind the distroless
# static base can run. -trimpath keeps build-machine paths out of panics, which
# would otherwise leak a developer's home directory into production logs.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE}" \
      -o /out/server ${MAIN_PKG}

# The final image carries no shell, no package manager and no libc. There is
# nothing for an attacker who reaches code execution to pivot into, and nothing
# for a CVE scanner to find, because the only file is our binary. Templates and
# CSS are compiled into it by go:embed, so there is no asset directory to mount
# or to get wrong.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server

# nonroot is uid 65532. Cloud Run does not require it, but a container that
# cannot write to its own filesystem cannot be persuaded to.
USER nonroot:nonroot

EXPOSE 8080
ENTRYPOINT ["/server"]
