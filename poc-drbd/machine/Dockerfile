FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y \
    drbd-utils \
    btrfs-progs \
    docker.io \
    docker-buildx \
    openssh-server \
    openssh-client \
    util-linux \
    kmod \
    jq \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Set up passwordless SSH between machines
RUN ssh-keygen -t ed25519 -f /root/.ssh/id_ed25519 -N "" \
    && cp /root/.ssh/id_ed25519.pub /root/.ssh/authorized_keys \
    && chmod 600 /root/.ssh/authorized_keys \
    && echo "StrictHostKeyChecking no" >> /root/.ssh/config \
    && echo "UserKnownHostsFile /dev/null" >> /root/.ssh/config \
    && chmod 600 /root/.ssh/config

# Configure sshd
RUN mkdir -p /run/sshd

# Copy scripts
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY demo.sh /usr/local/bin/demo.sh
COPY container-init.sh /usr/local/bin/container-init.sh
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/demo.sh /usr/local/bin/container-init.sh

# Copy app-container build context
COPY app-container/ /opt/app-container/

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
