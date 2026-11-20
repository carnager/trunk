# trunk

<img src="assets/trunk-icon.svg" width="96" align="right" alt="trunk">

A self-hosted photo and video server. Think immich, but one Go binary and a SQLite file. It
comes with a web client, and the Android app is called peek.

## Features

- Upload from phone or browser, resumable, deduplicated by SHA-256
- Timeline, albums, favourites, trash
- Sharing within the household, sending photos to someone, public links
- Search by place, date, file name and caption, and with `trunk-ai` by content ("dog on the beach") and faces
- Duplicate and look-alike finder
- Motion photos
- Fix wrong dates and places
- Google Takeout import
- Offline place names (GeoNames, built in)
- Daily database backups, `verify`, `reindex`

Your files stay plain files in `originals/<user>/<year>/<month>/`, readable without trunk.

## Install

Arch Linux: `packaging/arch` has a PKGBUILD. It installs `trunk`, `trunk-ai`, system and user units
for both, and a `trunk` user with its data in `/var/lib/trunk`.

```sh
sudo -u trunk trunk user add alice -admin -data /var/lib/trunk
sudo systemctl enable --now trunk
```

Or as a user service, with the data in `~/.local/share/trunk`:

```sh
systemctl --user enable --now trunk
trunk user add alice -admin -data ~/.local/share/trunk
loginctl enable-linger $USER    # keep it running when you log out
```

From source (Go 1.27):

```sh
make
./trunk user add alice -admin
./trunk serve -data /srv/trunk -listen :8420
```

trunk listens on plain HTTP. Put a reverse proxy in front for TLS and pass `-trust-proxy`;
`contrib/Caddyfile` does that with Caddy. Connect the app through the public address, because
public links use it.

`ffmpeg` and `ffprobe` are optional: with them, videos get poster frames, duration and location.
HEIC and AVIF work without any system libraries.

## Data directory

```
trunk.db       the database
originals/     your photos and videos
thumbs/        thumbnails, rebuilt when missing
backups/       a copy of trunk.db every day, the last 7
uploads/       unfinished uploads
ai/            the search text model the app downloads, uploaded by trunk-ai
```

Back up `originals/` and `backups/`. If you lose the database: copy the newest backup to
`trunk.db`. If there's no backup either, `trunk user add` the same users again and run
`trunk reindex`. Albums and sharing are gone then, but the photos come back with date, place and
camera.

The commands take `-data DIR` (default `./data`, or `$TRUNK_DATA`):

```sh
trunk backup /mnt/backup/trunk.db   # consistent copy, also while trunk runs
trunk verify                        # all files there?
trunk verify -hash                  # ...and unchanged?
```

## Google Takeout

Export Google Photos as .zip or .tgz and point trunk at the archives. There's no need to unpack
them.

```sh
trunk import takeout -user alice -dry-run ~/Downloads/takeout-*.tgz
trunk import takeout -user alice ~/Downloads/takeout-*.tgz
```

The import keeps dates, places, favourites, albums and edited versions. Running it again only
adds what's missing. Google sometimes strips the video from motion photos and exports it
separately; `-motion` puts it back.

## Content search and faces

trunk doesn't run AI models itself. `trunk-ai` ([ai/](ai/README.md)) does: it computes vectors
for photos and faces and sends them to trunk. Run it on the server if it's beefy enough (about
1 GB of RAM), or on a desktop now and then. Everything else works without it.

## More

- API: [docs/api.md](docs/api.md)
- Logo and colours: `assets/`

## License

GPL-3.0-or-later. Place data from [GeoNames](https://www.geonames.org) (CC BY 4.0), fonts under
the SIL Open Font License.
