# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT

# The developer image: compiles arcad inside the image so `docker build .`
# from a checkout is enough. The release image of spec 016, Dockerfile.ci,
# copies a binary the pipeline already built and signed; its runtime stage
# is the block between the markers below, byte for byte, and
# TestRuntimeStagesMatch is what keeps the two from drifting.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-X latere.ai/x/arca/internal/version.Version=${VERSION} -X latere.ai/x/arca/internal/version.Commit=${COMMIT} -X latere.ai/x/arca/internal/version.Date=${DATE}" \
      -o /out/arcad ./cmd/arcad

# >>> shared runtime base <<<
# arcad forks no binary of its own, so the runtime stage is distroless:
# CA roots for the bucket, the issuers, and the authorizer it dials,
# nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
EXPOSE 8080 8081
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/arcad"]
# <<< shared runtime base >>>

# The binary this image compiled. It is outside the markers because the two
# images get their binary from different places, which is the whole of the
# difference between them: here from the build stage above, in
# Dockerfile.ci from an archive the pipeline already built and signed.
COPY --from=build /out/arcad /usr/local/bin/arcad
