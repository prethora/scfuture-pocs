FROM alpine:3.20

RUN apk add --no-cache btrfs-progs

COPY container-init.sh /usr/local/bin/container-init.sh
RUN chmod +x /usr/local/bin/container-init.sh

ENTRYPOINT ["/usr/local/bin/container-init.sh"]
