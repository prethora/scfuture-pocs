FROM alpine:latest

RUN apk add --no-cache btrfs-progs

RUN adduser -D -h /workspace appuser

COPY container-init.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/container-init.sh

ENTRYPOINT ["/usr/local/bin/container-init.sh"]
