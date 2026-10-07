# =============================================================================
# VPN Gate pool refresher for the vpngate sidecar.
# Build from the repository root:
#   docker build -f deploy/vpngate/refresher/Dockerfile -t sub2api-vpngate-refresh:local .
# =============================================================================

ARG PYTHON_IMAGE=python:3.11-alpine
FROM ${PYTHON_IMAGE}
ARG PIP_INDEX_URL=https://pypi.org/simple

# tzdata: zoneinfo needs the IANA database; Alpine images do not ship it.
# /data/pool is created owned by uid 1000 so a fresh vpngate_pool volume
# (initialised from whichever image mounts it first) is writable here.
RUN apk add --no-cache tzdata && \
    pip install --no-cache-dir --index-url "${PIP_INDEX_URL}" PyYAML==6.0.3 && \
    addgroup -g 1000 vpngate && \
    adduser -u 1000 -G vpngate -s /sbin/nologin -D vpngate && \
    mkdir -p /data/pool && \
    chown vpngate:vpngate /data/pool

COPY deploy/vpngate/refresher/refresh.py /app/refresh.py
COPY deploy/vpngate/refresher/third_party/ /app/third_party/
RUN chmod -R a+rX /app

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
USER vpngate
ENTRYPOINT ["python", "/app/refresh.py"]
