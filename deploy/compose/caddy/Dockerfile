# HTTPS front for Yumdrake: the official Caddy image, run as a non-root user.
FROM caddy:2.10.2-alpine
RUN mkdir -p /data /config && chown -R 10001:10001 /data /config
USER 10001:10001
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=6 \
    CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:2019/config/"]
