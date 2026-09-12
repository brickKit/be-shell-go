# be-shell-go 不是 brickKit 组件（不进 brickkit.yaml），但基底同样要
# 带 shell（wget/curl），因为 be-ops 产出 8（shell-compose.yml）会给它
# 配一个 CMD-SHELL 健康检查，判据同任何组件容器一致（§12.3.7）。
FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shell ./cmd/shell

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/shell /app/shell
ENTRYPOINT ["/app/shell"]
