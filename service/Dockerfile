FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go test ./...
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /out/deploy-service .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && addgroup -S deploy && adduser -S -G deploy deploy && mkdir -p /var/lib/s2work-deploy && chown deploy:deploy /var/lib/s2work-deploy
COPY --from=build /out/deploy-service /usr/local/bin/deploy-service
USER deploy
ENV DEPLOY_DB=/var/lib/s2work-deploy/deploy.db LISTEN_ADDR=0.0.0.0:8096 TZ=Asia/Shanghai
ENTRYPOINT ["/usr/local/bin/deploy-service"]
