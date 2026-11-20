# trunk

<img src="assets/trunk-icon.svg" width="96" align="right" alt="trunk">

Self-hosted home for your photos and videos: a small Go alternative to immich. One binary, no
Node.js, no zoo of services.

trunk is the server; [peek](../peek) is the Android app. Both share one mascot: a little
elephant, because an elephant never forgets. In peek it peeks over a photo; in trunk it sits in
its travel chest full of pictures. *Trunk* means both: the elephant's trunk and the chest you
keep memories in.

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
