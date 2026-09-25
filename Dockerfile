# syntax=docker/dockerfile:1
#
# 构建阶段跑在构建机原生架构上（$BUILDPLATFORM），用 GOARCH 交叉编译出目标架构的静态二进制；
# 最终阶段只有 COPY、没有 RUN，所以出 arm64 也不需要 QEMU。
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/pushme ./cmd/pushme \
 && mkdir -p /out/data

# distroless/static 自带 CA 证书（访问 open.feishu.cn 要用），无 shell、无包管理器。
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pushme /pushme
# 没挂卷时 /data 也要能写（nonroot = 65532）；挂卷时宿主目录属主需是 65532。
COPY --from=build --chown=65532:65532 /out/data /data
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/pushme"]
