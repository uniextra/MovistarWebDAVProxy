FROM python:3.11-slim

ENV DEBIAN_FRONTEND=noninteractive
ENV DISPLAY=:99
ENV PYTHONUNBUFFERED=1

# Instalar dependencias para escritorio virtual (Xvfb, VNC, noVNC), herramientas y Playwright
RUN apt-get update && apt-get install -y --no-install-recommends \
    xvfb \
    x11vnc \
    novnc \
    websockify \
    fluxbox \
    procps \
    chromium \
    libnss3 libnspr4 libatk1.0-0 libatk-bridge2.0-0 \
    libcups2 libdrm2 libxkbcommon0 libxcomposite1 \
    libxdamage1 libxfixes3 libxrandr2 libgbm1 libasound2 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt
RUN pip install --no-cache-dir playwright

COPY . /app/

# Script de arranque: garantizar terminaciones LF y permisos de ejecución
RUN sed -i 's/\r$//' /app/start.sh && chmod +x /app/start.sh

VOLUME ["/data", "/app/tokens"]

# 5000: Web App, 8084: noVNC, 8080: WebDAV Proxy
EXPOSE 5000 8084 8080

ENTRYPOINT ["/app/start.sh"]
CMD ["python", "/app/app.py"]
