#!/bin/sh
# Start im Container: legt beim ersten Mal die Verzeichnisse und die
# Einstellungen an, gleicht das Archiv ab, startet OCR und Abgleich im
# Hintergrund und dann den Webdienst.
set -eu
H=$ARCHIV_HOME
mkdir -p "$H/data" "$H/html" "$H/eingang" "$H/backup"
[ -f "$H/data/archiv.conf" ] || cp /opt/archiv/examples/archiv.conf "$H/data/archiv.conf"
[ -f "$H/data/taxonomy.conf" ] || cp /opt/archiv/examples/taxonomy.conf "$H/data/taxonomy.conf"

/opt/archiv/bin/reindex.sh || echo "Abgleich fehlgeschlagen, siehe $H/data/reindex.log" >&2

# Abgleich in Abständen, OCR als eigener Prozess
( while sleep "$((ARCHIV_REINDEX_HOURS * 3600))"; do /opt/archiv/bin/reindex.sh || true; done ) &
python3 /opt/archiv/bin/ocr-pdfs.py --daemon >> "$H/data/ocr.log" 2>&1 &

exec archiv serve -config "$H/data/archiv.conf" -listen :8095
