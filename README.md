# trunk

<img src="assets/trunk-icon.svg" width="96" align="right" alt="trunk">

Self-hosted home for your photos and videos: a small Go alternative to immich. One binary, no
Node.js, no zoo of services.

trunk is the server; [peek](../peek) is the Android app. Both share one mascot: a little
elephant, because an elephant never forgets. In peek it peeks over a photo; in trunk it sits in
its travel chest full of pictures. *Trunk* means both: the elephant's trunk and the chest you
keep memories in.

## Running

```sh
make                                   # static binary ./trunk, no cgo
./trunk user add alice -admin          # asks for a password
./trunk serve -data /srv/trunk -listen :8420
```

trunk is one file. Optional: `ffmpeg`/`ffprobe` on `PATH` for video poster frames, duration and
location; without them videos are still stored and served. HEIC and AVIF decode through
built-in WebAssembly, so no system libraries are needed. Put a reverse proxy in front for TLS
and pass `-trust-proxy`.

Everything lives in the data directory:

```
data/
  trunk.db                     SQLite index (users, devices, assets, sync sequence)
  originals/alice/2024/06/…    your files, as uploaded, named as on the phone
  thumbs/                      JPEG renditions, rebuildable
  uploads/                     unfinished uploads
```

### Moving from Google Photos

Ask [Google Takeout](https://takeout.google.com) for Google Photos only, as .zip or .tgz files,
and point trunk at them; no need to unpack:

```sh
./trunk import takeout ~/Downloads/takeout-*.tgz -user alice -dry-run   # see what it will do
./trunk import takeout ~/Downloads/takeout-*.tgz -user alice
```

A .tgz can only be read front to back, so trunk reads each one twice: once to list it and collect
Google's metadata, once to import. Files are unpacked straight into the data directory, so the
disk needs room for the photos, not for another copy of the export.

It can run while the server is up. Dates and places come from the photos themselves and, where
they lack them, from Google's JSON files (which also give photos without an offset their time
zone). Favourites stay favourites, albums become trunk albums, and Google's "-edited" copies
become edits of their originals, so both versions are kept. Running it again only adds what is
missing, e.g. after a broken download.

Motion photos: Google exports their video part a second time as a file of its own (`X.MP` next to
`X.MP.jpg`, `X.MP4` next to `X.jpg` from older Pixels). Usually the photo still has it, so these
are skipped (`.MP`) or imported as short videos (`.MP4`). Sometimes the export drops the video
from the photo; with `-motion` trunk attaches such files to their photos instead, where the photo
lacks its video, and leaves out the copies. It can be run over an earlier import.

The originals tree is the source of truth and stays readable without trunk. Back up
`originals/` and `trunk.db`; thumbnails can be regenerated.

The API is described in [docs/api.md](docs/api.md).

## Running it for real

`contrib/trunk.service` runs trunk as a hardened systemd service under its own user, with the data
in `/var/lib/trunk`; `contrib/Caddyfile` puts it behind Caddy with HTTPS. Instructions are in both
files. Public links use the address peek is connected with, so connect peek through the public
name (`https://photos.example.org`), not the LAN address, for links that work anywhere.

### Backups

Back up the data directory with whatever you use (restic, borg, rsync). What matters in it:

- `originals/`: your photos and videos, as plain files. Losing these is losing photos.
- `backups/`: trunk copies its database there once a day (the last 7), consistently, so a backup
  taken while trunk is writing still has a usable database. The database holds what is not in the
  files: albums, captions, favourites, sharing, links, accounts.

`thumbs/` can be left out: trunk renders thumbnails again when they are missing.

```sh
trunk backup /mnt/backup/trunk.db     # a consistent database copy by hand, also while trunk runs
trunk verify                          # every file there and of the right size
trunk verify -hash                    # … and unchanged: reads the whole library
```

Restoring: put `originals/` back, copy the newest `backups/trunk-*.db` to `trunk.db`, start
trunk, run `trunk verify`. If the database is lost altogether, `trunk reindex` takes the files in
`originals/` back in where they are, with date, place and camera; the accounts have to be created
again first (`trunk user add`), with the same names as the folders in `originals/`.

## Status

Done: accounts and per-device tokens, simple and resumable (tus) upload with SHA-256
deduplication, metadata (EXIF, QuickTime, file names), thumbnails and video posters, delta sync,
favourites, trash with expiry, edits that keep the first upload, albums, Google Takeout import,
place names and captions, albums shared within the household, photos sent directly to someone,
public links with a small web page, daily database backups, `verify` and `reindex`.

Places: photos with GPS get the nearest town, region and country, offline, from
[GeoNames](https://www.geonames.org) data (CC BY 4.0) built into the binary
(`go run ./tools/placesgen` rebuilds it).

Next: search by content and faces.

## Brand

| | peek (app) | trunk (server) |
|---|---|---|
| Role | looks, shows | keeps, remembers |
| Icon | elephant peeking over a photo | the same elephant in an open travel chest |
| Ground | sun yellow `#FFC93C` | night violet `#241A5C` |
| Line | "Schau rein." | "Vergisst nichts." |

Shared colours: elephant `#5B3FD9`, ears `#4429B8`, chest `#FFB547` / `#E58F1F`, coral `#FF7A59`,
sky `#9ED8F5`, grass `#2BB673`, ink `#1E1B3A`. Wordmark set in **Righteous**, lower case.

`assets/`:
- `trunk-icon.svg` (rounded), `trunk-icon-square.svg` (full bleed), `trunk-icon-mono.svg` (single colour, for favicons)
- `elephant-mark.svg`: just the head, for the wordmark and small spaces
- `concept-board.html`: the full concept sheet with both products

## What peek needs from trunk

These are requirements from the client side, not an API design; the API takes shape while the
server is built.

- **Delta sync**: ask for everything that changed since a cursor or timestamp (new, edited,
  deleted, trashed, favourited), paged, so the app never re-lists the whole library.
- **Deduplication**: before uploading, the app sends a SHA-256 and learns whether the server
  already has the file.
- **Resumable uploads** for large videos over mobile networks.
- **Thumbnails in fixed sizes**: around 300 and 480 px for the grid, a screen-sized preview for
  the viewer, the original on demand. Video gets a poster frame.
- **Metadata with every asset**, so peek can show the info sheet and the map without downloading
  the original: taken-at time (with offset), width, height, orientation, MIME type, size, video
  duration, GPS (lat, lon, altitude), camera make and model, lens, aperture, exposure, ISO,
  focal length.
- **Favourites and trash** as server state; trash with an expiry date, restore and delete for good.
- **Albums** as server objects that are lists of asset ids, in order. peek already keeps its
  albums that way (`gallery/UserAlbums.kt`), so they map one to one.
- **Auth**: one token per device, listed and revocable individually, so a lost phone can be
  locked out without logging out everything else.
- **Later**: free-text search (CLIP embeddings) and face grouping on the server. Models run via
  ONNX Runtime through cgo; no Python sidecar.

peek keeps working without a server: everything above is an addition to the local gallery, not
a replacement.
