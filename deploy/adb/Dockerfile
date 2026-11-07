# syntax=docker/dockerfile:1.7
FROM debian:bookworm-slim

ADD --checksum=sha256:d230f13842f60f782a8645f9c813f8f845bf36089ea7289f28c48f17979313f1 https://dl.google.com/android/repository/platform-tools_r37.0.1-linux.zip /tmp/platform-tools.zip
RUN apt-get update \
    && apt-get install --yes --no-install-recommends ca-certificates libusb-1.0-0 unzip \
    && unzip -q /tmp/platform-tools.zip -d /opt \
    && rm -f /tmp/platform-tools.zip \
    && apt-get purge --yes unzip \
    && apt-get autoremove --yes \
    && rm -rf /var/lib/apt/lists/*

ENV HOME=/root \
    ANDROID_USER_HOME=/root/.android \
    ADB_LIBUSB=1 \
    ADB_MDNS_AUTO_CONNECT=

VOLUME ["/root/.android"]
EXPOSE 5037/tcp
ENTRYPOINT ["/opt/platform-tools/adb"]
CMD ["-a", "-L", "tcp:5037", "nodaemon", "server"]
