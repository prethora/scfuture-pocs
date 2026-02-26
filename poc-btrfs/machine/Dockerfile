FROM ubuntu:24.04

RUN apt-get update && apt-get install -y \
    btrfs-progs \
    docker.io \
    util-linux \
    jq \
    curl \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /entrypoint.sh
COPY demo.sh /demo.sh
RUN chmod +x /entrypoint.sh /demo.sh

ENTRYPOINT ["/entrypoint.sh"]
