# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT

# The developer image: compiles arcad inside the image so `docker build .`
# from a checkout is enough. The release image of spec 014 copies a binary
# the pipeline already built and attested; its runtime stage is this one.

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
# CA roots for the bucket, the issuers, and the webhooks it dials,
# nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/arcad /usr/local/bin/arcad
EXPOSE 8080 8081
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/arcad"]
# <<< shared runtime base >>>
