FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/qoder2api ./cmd/qoder2api

FROM alpine:3.20

WORKDIR /app
ENV QODER_HOST=0.0.0.0 \
    QODER_PORT=8963

COPY --from=build /out/qoder2api /app/qoder2api

EXPOSE 8963
ENTRYPOINT ["/app/qoder2api"]
