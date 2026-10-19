FROM debian:12.12-slim

WORKDIR /app
COPY tokendance-gateway /app/tokendance-gateway
COPY config.yaml /app/config.yaml

EXPOSE 3002

ENTRYPOINT ["/app/tokendance-gateway", "/app/config.yaml"]
