FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app/server ./cmd/server

FROM alpine:3.21
WORKDIR /app
COPY --from=builder /app/server .
EXPOSE 9527
CMD ["./server"]
