# Archiv

Ein Webdienst für ein Archiv von Dokumenten, Software und Fotos – gebaut für
Sammlungen rund um klassische Computer, aber nicht darauf beschränkt. Er
läuft für das Archiv des Vereins zum Erhalt klassischer Computer e.V. und
genauso für die eigene Sammlung zu Hause.

- **Katalog mit Schlagworten**: Jede Datei gehört zu einem Objekt (ein
  Handbuch, ein Satz Disketten, ein Foto), Objekte tragen Schlagworte nach
  Facetten – Rechner (Hersteller › Serie › Modell), Art, Sprache, Quelle,
  Zeitschrift, Treffen. Gestöbert wird über die Schlagworte, die Ordner der
  Ablage bleiben daneben erhalten.
- **Volltextsuche** über Titel, Beschreibungen und den Text aller PDFs,
  seitenweise; Scans ohne Textebene liest die OCR (Tesseract).
- **Leser** für PDFs im Browser, der direkt auf der Seite eines Treffers
  aufschlägt.
- **Vorschaubilder** für Bilder und die erste Seite jedes PDFs.
- **Einreichungen**: Dateien hochladen (auch sehr große, Uploads setzen nach
  Abbrüchen wieder auf), beschreiben, einreichen. Eine KI (Claude von
  Anthropic, optional) schlägt Schlagworte und Beschreibungen vor und erkennt
  Dubletten; Moderatoren prüfen und nehmen auf.
- **Bearbeiten im Browser** mit vollständigem Verlauf: Wer nicht selbst
  bearbeiten darf, macht Vorschläge, über die Moderatoren entscheiden.
- **ZIP-Download** ganzer Ordner und Objekte.

Die Oberfläche ist deutsch.

## Schnellstart mit Docker

```sh
git clone https://github.com/vzekc/archiv.git
cd archiv
docker compose up -d
```

Das Archiv ist dann unter <http://localhost:8095> erreichbar. Beim ersten
Start legt der Container im Verzeichnis `archiv/` neben `compose.yaml` an:

| Pfad | Inhalt |
|---|---|
| `archiv/html/` | die Dateien des Archivs – hier hinein kopiert man die Sammlung, in beliebigen Ordnern |
| `archiv/data/archiv.conf` | die Einstellungen (siehe unten) |
| `archiv/data/taxonomy.conf` | die festen Schlagworte |
| `archiv/data/*.sqlite` | Katalog, Volltext, Einreichungen |
| `archiv/eingang/` | Uploads, bis sie aufgenommen sind |

Der Container gleicht beim Start und danach alle 24 Stunden das Archiv ab:
neue und geänderte Dateien kommen in den Katalog, PDFs werden ausgelesen,
Vorschaubilder entstehen. Die OCR läuft daneben ständig und arbeitet Scans
ohne Textebene nach und nach ab. Wer Dateien direkt in `archiv/html/` legt
und nicht warten will, stößt den Abgleich von Hand an:

```sh
docker compose exec archiv /opt/archiv/bin/reindex.sh
```

Ohne Einstellungen gibt es keine Anmeldung: Jeder, der die Seite erreicht,
ist „Archivar“ und darf alles. Das passt für einen Rechner im eigenen Netz;
wer das Archiv ins Internet stellt, braucht eine Anmeldung (siehe
[Anmeldung](#anmeldung)).

## Einstellungen

Die Datei `data/archiv.conf` enthält eine Einstellung je Zeile,
`schlüssel = wert`; Zeilen mit `#` sind Kommentare. Eine kommentierte
Vorlage liegt in [`examples/archiv.conf`](examples/archiv.conf).

| Schlüssel | Bedeutung | Vorgabe |
|---|---|---|
| `site_url` | Adresse des Archivs, für Links | `http://localhost:8095` |
| `site_title` | Name im Seitenkopf | Hostname aus `site_url` |
| `site_tagline` | Zeile neben dem Namen | leer |
| `home_title` | Überschrift der Startseite | `site_title` |
| `archive_name` | wie die KI das Archiv nennt, z. B. „Archiv der Familie Müller“ | `Archiv` |
| `auth` | `single` oder `header`, siehe [Anmeldung](#anmeldung) | `single` |
| `single_user` | bei `auth = single`: Name der einen Person | `Archivar` |
| `moderator_group` | Gruppe der Moderatoren | `Moderatoren` |
| `edit_group` | Gruppe, die den Katalog direkt bearbeitet; alle anderen schlagen vor | `Moderatoren` |
| `tag_create_group` | Gruppe, die neue Schlagworte anlegt | `Moderatoren` |
| `intake` | `direct`: eigene Einreichungen von Moderatoren gehen nach der KI gleich zur Aufnahme; `moderated`: alle laufen über die Liste der Moderation | `moderated` |
| `editing` | `on`: Katalog im Browser bearbeiten | aus |
| `api_key_file` | Datei mit dem API-Schlüssel von Anthropic; ohne sie gibt es keine KI-Vorschläge | `data/anthropic.key` |
| `model` | das Claude-Modell | `claude-opus-5-5` |
| `max_file_gb`, `max_submission_gb`, `max_pending_gb` | Grenzen je Datei, je Einreichung, offen je Person | 20, 50, 100 |
| `thumbs` | Vorschaubilder nach Prüfsumme (`hash`) oder nach Pfad (`path`) ablegen | `hash` |
| `home` | die Installation; alle Pfade liegen darunter | `$ARCHIV_HOME`, sonst das Arbeitsverzeichnis |

Die Pfade (`archive`, `catalog`, `pages`, `db`, `staging`, `taxonomy` …)
lassen sich einzeln setzen; ohne Angabe liegen sie unter `home` wie in der
Tabelle im Schnellstart.

### Schlagworte

`data/taxonomy.conf` legt die Schlagworte fest, die immer da sein sollen –
vor allem die Arten (Handbuch, Software, Foto …) und die Rechner. Das Format
steht am Anfang der [Vorlage](examples/taxonomy.conf). Weitere Schlagworte
entstehen beim Arbeiten: beim Aufnehmen von Einreichungen und beim
Bearbeiten im Browser.

### KI-Vorschläge

Mit einem API-Schlüssel von Anthropic in `data/anthropic.key` ordnet Claude
Einreichungen ein: Es gruppiert die Dateien, schlägt Schlagworte und
Beschreibungen vor und nennt Auffälligkeiten. Entschieden wird immer von
Menschen. Der Befehl `archiv classify` schlägt außerdem Schlagworte für
Objekte vor, die noch keine haben – nützlich, wenn eine große, bisher nur in
Ordnern sortierte Sammlung ins Archiv kommt. Die Aufrufe kosten Geld nach
den Preisen von Anthropic.

## Anmeldung

**`auth = single`** – keine Anmeldung. Jeder Aufruf ist dieselbe Person, die
moderiert und bearbeitet. Für ein Archiv, das nur im eigenen Netz erreichbar
ist.

**`auth = header`** – ein Proxy vor dem Archiv meldet an und gibt Kennung,
Namen und Gruppen der Person als Kopfzeilen weiter. Erprobt ist Apache mit
`mod_auth_openidc` gegen einen OpenID-Connect-Anbieter; jeder andere, der
Kopfzeilen setzt, geht genauso. Die Namen der Kopfzeilen sind einstellbar
(`user_header`, `name_header`, `groups_header`). Der Dienst muss dann so
laufen, dass er nur über den Proxy erreichbar ist (`listen = 127.0.0.1:8095`),
und der Proxy muss diese Kopfzeilen aus dem entfernen, was der Browser
schickt – `mod_auth_openidc` tut das.

Ein Beispiel für Apache:

```apache
<VirtualHost *:443>
    ServerName archiv.example.org
    DocumentRoot /srv/archiv/html

    OIDCProviderMetadataURL https://login.example.org/.well-known/openid-configuration
    OIDCClientID archiv
    OIDCClientSecret …
    OIDCRedirectURI https://archiv.example.org/oidc/redirect
    OIDCCryptoPassphrase …
    OIDCScope "openid profile groups"

    <Location />
        AuthType openid-connect
        Require valid-user
    </Location>

    # Vorhandene Dateien liefert Apache selbst, alles andere der Dienst
    RequestHeader set X-Forwarded-Proto "https"
    RewriteEngine on
    RewriteCond %{REQUEST_URI} !^/oidc/
    RewriteCond %{DOCUMENT_ROOT}%{REQUEST_URI} !-f
    RewriteRule ^ http://127.0.0.1:8095%{REQUEST_URI} [P]
</VirtualHost>
```

## Ohne Docker

Gebraucht werden Go (zum Bauen), Python 3, `poppler-utils` (`pdftotext`,
`pdftoppm`, `pdfinfo`), ImageMagick (`convert`), `7zz` (7-Zip), `file` und
für die OCR Tesseract mit den Sprachen `deu` und `eng`. Optional prüft
`clamdscan` Uploads auf Viren.

```sh
cd server
go build -o archiv ./cmd/archiv
```

Eine Installation ist ein Verzeichnis mit `bin/` (die Skripte aus diesem
Repository), `server/archiv` (das Programm), `data/`, `html/` und `eingang/`.
Dann:

```sh
export ARCHIV_HOME=/srv/archiv
cp examples/archiv.conf examples/taxonomy.conf $ARCHIV_HOME/data/
$ARCHIV_HOME/bin/reindex.sh                 # erster Abgleich
$ARCHIV_HOME/server/archiv serve            # der Webdienst
```

Dauerhaft laufen der Webdienst und `bin/ocr-pdfs.py --daemon` als Dienste
(systemd), `bin/reindex.sh` einmal am Tag per cron. Die Skripte finden die
Installation über `$ARCHIV_HOME` oder, ohne diese Variable, im Verzeichnis
über `bin/`. Liegt das Programm woanders als in `server/archiv`, sagt
`data/archiv.env` es `reindex.sh` (`ARCHIV_SERVER=/pfad/zum/archiv`).

## Befehle

```
archiv serve                       der Webdienst (Vorgabe)
archiv sync                        Katalog mit den Dateien abgleichen (macht reindex.sh)
archiv classify                    Schlagworte für Objekte ohne vorschlagen (KI)
archiv export-descriptions <dir>   Katalog als descriptions-Dateien schreiben
archiv split-item, merge-gone      Objekte teilen, entfernte Dateien zusammenführen
```

Alle nehmen `-config <datei>`; ohne gilt `$ARCHIV_HOME/data/archiv.conf`.

## Erweiterungen

Das Programm ist eine Go-Bibliothek mit einem kleinen `main`
([`server/cmd/archiv`](server/cmd/archiv/main.go)). Wer das Archiv an seine
Umgebung anbinden will – Benachrichtigungen per Mail oder Chat, Namen aus
einem Benutzerverzeichnis, eigene Seiten –, schreibt ein eigenes `main`, das
`archiv.Main(…)` mit Erweiterungen aufruft. Was eine Erweiterung kann, steht
in [`server/extension.go`](server/extension.go): Ereignisse zu Einreichungen
empfangen, Nachrichten zustellen, Namen nachschlagen, im Hintergrund
arbeiten, Seiten und Vorlagen beitragen und eigene Einstellungen lesen.

## Entwicklung

```sh
cd server
go test ./...
go run ./cmd/archiv serve -config ../examples/archiv.conf
```

Die Tests bringen ein kleines Archiv in `server/testdata` mit.

## Lizenz

MIT, siehe [LICENSE](LICENSE). Mitgeliefert sind pdf.js (Apache-2.0) und
Uppy (MIT) in `server/static`.
