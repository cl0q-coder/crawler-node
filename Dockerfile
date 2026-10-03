# syntax=docker/dockerfile:1
# cl0q crawler node. Every service is a static Go binary (CGO off) in one small image; docker-compose.yml
# picks the binary per service with `command:`.

##############################  build  ##############################
FROM golang:1.25-bookworm AS build
WORKDIR /src

# Module downloads first, so editing source doesn't re-download dependencies.
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN set -eux; mkdir -p /out; \
    for c in website-crawler domain-crawler dns-enricher asn-enricher cl0q-node; do \
        CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "/out/$c" "./cmd/$c"; \
    done

#############################  runtime  #############################
# distroless/static: CA certificates + tzdata, no shell, runs as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY seeds.txt /app/seeds.txt
WORKDIR /app
USER nonroot:nonroot
