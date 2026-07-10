# Build a fully static single binary with the web UI embedded.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/r2proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/r2proxy /r2proxy
# 8080 = S3 data-plane proxy, 8081 = admin API + web console
EXPOSE 8080 8081
VOLUME ["/data"]
ENV R2PROXY_CONFIG=/data/r2proxy.json \
    R2PROXY_LISTEN=0.0.0.0:8080 \
    R2PROXY_ADMIN_LISTEN=0.0.0.0:8081
ENTRYPOINT ["/r2proxy"]
CMD ["serve"]
