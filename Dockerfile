# Das Archiv in einem Container: Webdienst, nächtlicher Abgleich und OCR.
# Daten und Dateien liegen im Volume /archiv (siehe README).
FROM golang:1.27-trixie AS build
WORKDIR /src
COPY server/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/archiv ./cmd/archiv

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 poppler-utils imagemagick 7zip file ca-certificates \
        tesseract-ocr tesseract-ocr-deu tesseract-ocr-eng \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/archiv /usr/local/bin/archiv
COPY bin/ /opt/archiv/bin/
COPY examples/ /opt/archiv/examples/
COPY docker/start.sh /opt/archiv/start.sh
ENV ARCHIV_HOME=/archiv \
    ARCHIV_SERVER=/usr/local/bin/archiv \
    ARCHIV_REINDEX_HOURS=24
VOLUME /archiv
EXPOSE 8095
CMD ["/opt/archiv/start.sh"]
