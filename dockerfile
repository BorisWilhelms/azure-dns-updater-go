FROM golang:1.20-alpine AS build

WORKDIR /app

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY . ./

RUN go build .

FROM alpine:3.17 as runtime

RUN adduser --disabled-password --no-create-home app

USER app
WORKDIR /app

COPY --from=build /app/azure-dns-updater-go .

ENTRYPOINT ["/app/azure-dns-updater-go"]
