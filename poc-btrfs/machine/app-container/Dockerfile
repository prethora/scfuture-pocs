FROM alpine:3.19
RUN apk add --no-cache btrfs-progs
COPY container-init.sh /container-init.sh
RUN chmod +x /container-init.sh
ENTRYPOINT ["/container-init.sh"]
