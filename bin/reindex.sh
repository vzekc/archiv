#!/bin/sh
#
# Nächtlicher Abgleich des Archivs: Katalog, Dateiindex, Volltext und
# Vorschaubilder. Läuft per cron (oder im Docker-Container per Schleife),
# kann aber jederzeit von Hand gestartet werden.
#
# Bei Erfolg schweigt das Skript (Ausgabe nur ins Log), damit cron keine Mail
# verschickt; im Fehlerfall geht die Ausgabe nach stderr.
#
# Die Installation ist $ARCHIV_HOME, sonst das Verzeichnis über bin/. Liegt
# dort data/archiv.env, setzt sie abweichende Werte (Shell-Syntax):
#   ARCHIV_SERVER   das Programm des Webdienstes (Standard: server/archiv,
#                   sonst archiv im PATH)
#   ARCHIV_CONF     seine Einstellungen (Standard: data/archiv.conf)
#
set -eu

BIN=$(cd "$(dirname "$0")" && pwd)
ARCHIV_HOME=${ARCHIV_HOME:-$(dirname "$BIN")}
export ARCHIV_HOME
if [ -f "$ARCHIV_HOME/data/archiv.env" ]; then
	. "$ARCHIV_HOME/data/archiv.env"
fi
ARCHIV_CONF=${ARCHIV_CONF:-$ARCHIV_HOME/data/archiv.conf}
if [ -z "${ARCHIV_SERVER:-}" ]; then
	if [ -x "$ARCHIV_HOME/server/archiv" ]; then
		ARCHIV_SERVER=$ARCHIV_HOME/server/archiv
	else
		ARCHIV_SERVER=archiv
	fi
fi
LOG=$ARCHIV_HOME/data/reindex.log
LOCK=$ARCHIV_HOME/data/reindex.lock

stamp() { date '+%Y-%m-%d %H:%M:%S'; }

# Nicht zwei Läufe gleichzeitig: der Indexer schreibt eine neue Datenbank und
# die anderen Schritte lesen sie.
exec 9>"$LOCK"
if ! flock -n 9; then
	echo "$(stamp) uebersprungen: ein Lauf ist noch aktiv" >> "$LOG"
	exit 0
fi

started=$(date +%s)

# Reihenfolge: erst den Katalog abgleichen (liefert die Pruefsummen, an denen
# Volltext und Vorschaubilder verschobener Dateien haengen), dann indizieren
# (holt vorhandene Volltexte aus dem Speicher), dann neue Dokumente auslesen,
# dann Vorschaubilder. OCR laeuft nicht mit: das dauert Stunden und laeuft als
# eigener Dienst (bin/ocr-pdfs.py --daemon).
LAYOUT=hash
if grep -q '^thumbs *= *path' "$ARCHIV_CONF" 2>/dev/null; then
	LAYOUT=path
fi

if ! out=$( { "$ARCHIV_SERVER" sync -quiet -config "$ARCHIV_CONF" \
	&& "$BIN/index-collection.py" \
	&& "$BIN/extract-text.py" --jobs 4 \
	&& "$BIN/make-thumbs.py" --jobs 4 --layout "$LAYOUT"; } 2>&1 ); then
	{
		echo "$(stamp) FEHLER beim Abgleich:"
		echo "$out" | sed 's/^/    /'
	} >> "$LOG"
	echo "archiv: Abgleich fehlgeschlagen, siehe $LOG" >&2
	echo "$out" >&2
	exit 1
fi

took=$(( $(date +%s) - started ))
{
	echo "$(stamp) ok (${took}s)"
	echo "$out" | sed 's/^/    /'
} >> "$LOG"

# Log kurz halten
if [ -f "$LOG" ]; then
	tail -n 500 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
fi

exit 0
