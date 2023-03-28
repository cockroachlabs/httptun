# syntax=docker/dockerfile:1
FROM golang:1.19 AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN GOPROXY=https://proxy.golang.org,direct go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o server ./cmd/server

# Execution container
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /app/server /server

ENTRYPOINT ["/server"]
