FROM golang:1.22-alpine

WORKDIR /app
COPY . .

RUN go build -o parallel-image-build ./cmd/main.go

FROM alpine:latest

COPY --from=0 /app/parallel-image-build /usr/local/bin/parallel-image-build
COPY --from=docker/buildx-bin /buildx /usr/libexec/docker/cli-plugins/docker-buildx

RUN apk add --no-cache docker-cli

ENTRYPOINT [ "/usr/local/bin/parallel-image-build" ]