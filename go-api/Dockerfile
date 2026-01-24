# go-api/Dockerfile
FROM golang:1.22.12-alpine AS build


WORKDIR /app

# 의존성 먼저 캐시
COPY go.mod go.sum ./
RUN go mod download

# 소스 복사 후 빌드
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o go-api ./main.go

FROM alpine:3.20

WORKDIR /app
COPY --from=build /app/go-api /app/go-api

EXPOSE 8080
CMD ["/app/go-api"]
