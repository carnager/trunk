# trunk API v1

Everything is JSON over HTTP under `/api/v1`. trunk speaks plain HTTP; put it behind a reverse
proxy (Caddy, nginx) for TLS and start it with `-trust-proxy` so login rate limiting sees real
client addresses.

Errors are `{"error": "message"}` with a fitting status code. Times are RFC 3339 in UTC. IDs are
integers.

## Web client

`GET /` is trunk's own web client (`/app/…` its files), with a strict Content-Security-Policy. It
uses this API with the token in the `Authorization` header like any client; for `<img>` and
`<video>`, which cannot send headers, trunk also takes the token from a `trunk_token` cookie,
for GET and HEAD only, so another site cannot make a browser change anything.

## Auth

Each device logs in once and keeps its token. Tokens never expire; they are revoked one at a time.

| | |
|---|---|
| `POST /auth/login` | `{"user", "password", "device": "Pixel 8"}` → `{"token": "trk_…", "device_id"}`. 5 failures from one address within 15 min → `429` for the rest of the window. |
| `POST /auth/logout` | revokes the calling token. `204` |
| `GET /me` | `{"id", "name", "admin", "device_id"}` |
| `GET /devices` | `{"devices": [{"id", "name", "created_at", "last_seen_at", "current"}]}` |
| `DELETE /devices/{id}` | revoke another device of the same user (lost phone). `204` |

Send `Authorization: Bearer <token>` on every other request. `GET /server` needs no token and
reports `version`, `api`, `trash_days`, `max_upload`, `thumb_sizes` and `features`
(`video_posters` is false when the server has no ffmpeg).

## Assets

```json
{
  "id": 17, "seq": 342,
  "sha256": "9f86d0…", "name": "PXL_20230717_143721529.jpg",
  "kind": "image", "mime": "image/jpeg", "size": 2672692,
  "taken_at": "2023-07-17T14:37:21Z", "tz_offset": 120, "local_time": "2023-07-17T16:37:21",
  "width": 4032, "height": 3024, "orientation": 1,
  "lat": 52.52, "lon": 13.41, "alt": 34.1,
  "make": "Google", "model": "Pixel 5", "lens": "…",
  "f_number": 1.73, "exposure": 0.042, "iso": 622, "focal_length": 4.38,
  "place": {"city": "Wien", "country": "Österreich", "cc": "AT"},
  "caption": "Stephansdom im Schnee",
  "duration_ms": 32777,
  "edited_at": "…", "original_sha256": "…", "original_size": 3100000, "original_mime": "image/heic",
  "favorite": false, "trashed_at": null, "trash_expires_at": null,
  "created_at": "…", "updated_at": "…"
}
```

- Fields without a value are left out.
- **Width and height are display dimensions**: EXIF orientation is already applied, so use them as
  they are for the grid aspect ratio. `orientation` is informational.
- **Time.** If `tz_offset` (minutes east of UTC) is present, `taken_at` is the exact instant and
  `local_time` the wall clock where the photo was taken. Without it, the offset is unknown,
  `local_time` is what the camera showed, and `taken_at` is that same wall-clock time labelled
  UTC. Show `local_time` to people; sort by `taken_at`.
- **Place** is the nearest town (at least 1000 inhabitants, within 30 km) to `lat`/`lon`, looked
  up offline. Names follow the request's `Accept-Language` where GeoNames has them (German so
  far), English otherwise. `region` is left out when it only repeats the city (Wien, Berlin).
- **Caption** is free text, set with `PATCH`; a Google Takeout import brings Google's descriptions.
- Where the time comes from, in order: EXIF/QuickTime metadata, a timestamp in the file name
  (`IMG_20240131_142530`, `PXL_…`, `VID_…`, `Screenshot_…`, `signal-2024-01-31-…`), the
  client's `mtime`, upload time. For Android videos, which only store UTC, the offset is
  recovered by comparing against the local time in the file name.

| | |
|---|---|
| `GET /assets/{id}` | one asset |
| `PATCH /assets/{id}` | `{"favorite": true}` and/or `{"caption": "…"}` (empty clears it) → the updated asset |
| `GET /assets/{id}/file` | the current file (the edit, if there is one); supports `Range` for video seeking. Use this to show or share. |
| `GET /assets/{id}/original` | the file as first uploaded, even after edits |
| `GET /assets/{id}/motion` | a motion photo's video (`video/mp4`, ranges work); `404` for anything else. Assets that have one say `"motion": true` |
| `PUT /assets/{id}/edit` | raw bytes of an edited photo (`X-Sha256` optional) → the updated asset. See *Edits*. |
| `DELETE /assets/{id}/edit` | back to the first upload → the updated asset |
| `GET /assets/{id}/thumb/{size}` | JPEG, upright. `small`: short side 300 px; `medium`: short side 480 px; `preview`: long side 2048 px. Never upscaled. Videos get a poster frame from about 1 s in. `404` if the file cannot be rendered. Supports `If-None-Match`. |
| `POST /assets/trash` | `{"ids": [..]}` → `204` |
| `POST /assets/restore` | `{"ids": [..]}` → `204` |
| `POST /assets/delete` | `{"ids": [..]}` → `{"deleted": n}`. Only trashed assets are deleted; others are skipped. The trash is also emptied automatically after `trash_days`. |

## Edits

An edit replaces what a photo shows without losing the upload: the first upload stays on disk
next to the edit (`IMG_1234.heic`, `IMG_1234_edit.jpg`) and `/original` keeps serving it. Editing
again replaces the previous edit; only the first upload is kept forever.

After an edit, `sha256`, `size`, `mime`, `width`, `height`, `orientation` and the thumbnails
describe the edit, and `edited_at` plus `original_sha256`/`original_size`/`original_mime`
describe what was first uploaded. When and where the photo was taken and the camera data stay as
they were. The change gets a new `seq`, so other devices pick it up with their next sync.

`PUT` returns `409` with `asset_id` if the file is already another asset, `415` if it is not a
photo, and treats a file identical to the first upload as a revert. Videos cannot be edited.

## Albums

```json
{"id": 3, "seq": 360, "name": "Urlaub 2024", "asset_ids": [17, 4, 9], "created_at": "…", "updated_at": "…"}
```

An album is an ordered list of the user's own assets; the photos stay in the library when they
leave an album or the album is deleted. Albums always come with their complete list.

| | |
|---|---|
| `POST /albums` | `{"name", "asset_ids": [..]}` (ids optional) → `201` album |
| `GET /albums/{id}` | one album |
| `PATCH /albums/{id}` | `{"name"}` → album |
| `POST /albums/{id}/add` | `{"ids": [..], "end": false}` → album. Adds to the front (in the order given) or, with `end`, to the back. Ids already in the album keep their place. Only the caller's own live assets; otherwise `400`. |
| `POST /albums/{id}/remove` | `{"ids": [..]}` → album |
| `DELETE /albums/{id}` | `204` |

Trashed assets stay in their albums (clients hide them); deleting an asset for good removes it.

### Sharing within the household

Albums can be shared with the other accounts on the server. Members see the album in their
feed with `"role": "member"` and `"owner": {"id", "name"}`; the owner sees `"members": [{"id",
"name", "can_add"}]`. Members may add their own photos (unless `can_add` is false) and take out
what they added; only the owner renames, deletes, shares and takes out anything.

Photos of others are not in a member's own feed and library. Fetch them per album:

| | |
|---|---|
| `GET /users` | the other accounts: `{"users": [{"id", "name"}]}` |
| `PUT /albums/{id}/members/{user}` | `{"can_add": true}` shares the album or changes the right → album (owner) |
| `DELETE /albums/{id}/members/{user}` | stops sharing; a member may remove themselves to leave → `204` |
| `GET /albums/{id}/assets` | the album's assets in full, in album order; others' carry `"owner": "name"` |

Any change to a photo in a shared album (edit, caption, favourite, trash) gives the album a new
`seq`, so clients fetch its assets again when a shared album shows up in `/changes`. Shared
photos can be read (`/assets/{id}`, `/file`, `/original`, `/thumb`) but only changed by their
owner. A member who leaves or is removed gets the album as a tombstone.

### Sending photos to someone

`POST /shares {"user_id", "asset_ids": [..]}` → album. Photos go to the front of the one album of
`"kind": "direct"` from the caller to that person, made on first use and shared with them
view-only. Its `name` is the recipient's name. Remove photos from it to take them back, delete it
to stop sharing; the recipient can leave it (`DELETE /albums/{id}/members/{own id}`) to hide it,
and the next photo sent brings it back. Direct albums cannot be renamed or get more members.

### Public links

For people without an account. A link shows an album as it is now, or a fixed selection.

| | |
|---|---|
| `POST /links` | `{"album_id"}` or `{"asset_ids": [..]}`, optional `"title"`, `"strip_location"` (default true), `"expires_days"` → `201` link |
| `GET /links` | the caller's live links |
| `DELETE /links/{token}` | revoke → `204` |

A link is `{"token", "path": "/s/<token>", "title", "album_id"|"asset_ids", "strip_location",
"created_at", "expires_at"}`; its address is the server's address plus `path`. Only one's own photos
and albums can be linked, and an album link shows only the owner's photos in it, not what household
members added. The page at `/s/<token>` (no login) shows previews with date, town and caption, never
coordinates, and offers every original for download (what a browser shows it can save anyway).

With `strip_location`, originals go out without where they were taken: the GPS directory of JPEG
EXIF data and GPS fields in XMP are emptied, video location boxes (`©xyz`, `loci`) and Apple's
location key blanked, in place, so size, pixels, date and camera stay as they are and motion photos
keep working. Formats trunk cannot clean (HEIC, for one) are replaced by their preview JPEG when they
have a location. Trashed photos leave the link; revoked and expired links show nothing.

## Content search

| Call | |
|---|---|
| `GET /ai/model` | → `{"model", "files": [{"name", "size", "sha256"}], "indexed": n}`: the text model clients download to make search vectors; `404` while there is none |
| `GET /ai/model/{model}/{file}` | one of its files: `text.onnx`, `vocab.txt`, `dense.safetensors`, `model.json` |
| `POST /ai/search` | `{"model", "vec": base64, "limit"}` → `{"results": [{"id", "score"}]}`: one's own photos, best first |
| `GET /ai/todo?model=M&limit=N` | admin: `{"assets": [{"id", "sha256"}], "remaining": n}`, the household's files without a vector yet |
| `GET /ai/thumb/{id}` | admin: any asset's medium thumbnail, for the indexer |
| `PUT /ai/embeddings` | admin: `{"model", "items": [{"sha256", "vec": base64}]}` |
| `PUT /ai/model/{model}/{file}` | admin: a file of the text model; once all four are there it is the current one |

trunk runs no model: the machines it runs on are often too small. An indexer elsewhere (`trunk-ai`,
see `ai/`) fetches thumbnails, computes a vector per file content with an image model and sends it;
clients turn search text into a vector with the matching text model, which the indexer uploads to
trunk for them, and send that. Vectors are unit-length little-endian float32s (base64); trunk
compares them (dot product) and returns the best match and everything within 0.035 of it, at least
0.255 (tuned on CLIP ViT-B/32, multilingual). `features.content_search` says a model is there.

## People

| Call | |
|---|---|
| `GET /people` | → `{"people": [{"id", "name", "hidden", "faces", "cover", "asset_ids"}]}`, most photographed first |
| `PATCH /people/{id}` | `{"name"}` and/or `{"hidden"}` → `204` |
| `POST /people/{id}/merge` | `{"ids": [..]}`: those people are this one → `204` |
| `DELETE /people/{id}/faces/{face}` | not this person; the face does not go back to them → `204` |
| `GET /faces/{id}/thumb` | the face cut from its photo, 200×200 JPEG (`cover` is one) |
| `GET /assets/{id}/faces` | → `{"faces": [{"id", "box": [x, y, w, h], "person", "name"}]}`, boxes relative to the picture |
| `GET /ai/faces/todo?model=M` | admin: photos not looked at for faces |
| `PUT /ai/faces` | admin: `{"model", "items": [{"sha256", "faces": [{"box", "score", "frontal", "vec"}]}]}` |

trunk-ai finds faces (YuNet) and a vector per face (SFace, 128 numbers); trunk groups each user's
faces into people (DBSCAN: similarity ≥ 0.65, at least 3 frontal faces), some seconds after the
last faces came. Groups keep the person most of their faces had, so names stick as photos arrive;
merges and rejections are kept. Faces that fit nobody clearly (profiles, blur, people seen once)
stay out. People are per user: each names their own.

## Correcting date and place

| Call | |
|---|---|
| `GET /places?q=wien` | → `{"places": [{"id", "city", "region", "country", "cc", "lat", "lon"}]}`: towns by name, in any language; exact names first, then bigger towns. In the caller's language (`Accept-Language`) |
| `POST /assets/adjust` | `{"ids": [..]}` plus `"shift_minutes"`, or `"local_time"` (`2006-01-02T15:04:05`) with optional `"tz_offset"`; and/or `"place_id"` or `"clear_place": true` → `{"assets": [..]}` |

For scans, cameras with a wrong clock, files that lost their metadata. `local_time` sets the
earliest of the photos to that time and moves the others by the same amount, so a series stays in
order; `shift_minutes` moves them all. A `place_id` sets the town and drops a position the photos
had (it was wrong); `clear_place` leaves them without place and position. Only the database
changes, not the files, so their own metadata stays; the changes reach clients through the feed.
Only one's own photos.

## Motion photos

JPEGs with a short video appended (Pixel, Samsung and others) are recognised on upload, in both
the Motion Photo format (`Container:Directory` with the video's length) and the older MicroVideo
one. The video has to really be an MP4 running to the end of the file; when the stated length
does not fit (Google Photos keeps the original's in its edits) trunk finds the video itself.
Photos from before trunk knew motion photos are looked at once in the background, and the ones
found reach clients through the change feed. An edit is a plain picture; the video keeps coming
from the first upload. On public links that leave out the location, the video's location is
blanked as well.

## Look-alikes

| Call | |
|---|---|
| `GET /similar` | → `{"groups": [{"asset_ids": [..], "keep": id, "kind": "same"\|"burst"\|"motion"}], "pending": n}` |
| `POST /similar/ignore` | `{"ids": [..]}` → `204`: these are no look-alikes of each other |

Finds the user's own photos that look alike, to clean up a library: `same` is one picture in
several files (a WhatsApp copy, an export, an edit saved as a copy), `burst` are shots taken
moments apart, `motion` is a video that is byte for byte the video inside one of the user's motion
photos (Takeout exports it apart as well); its group is the photo and the video, keeping the photo.
Byte-identical files never get this far; uploads are deduplicated. Each photo is
fingerprinted from its thumbnail (a 512-bit difference hash plus how much detail it has) after
upload, and in the background for older ones; `pending` counts those not done yet. Every photo in
a group looks like every other one. `keep` is a suggestion: most pixels, then edited, favourite,
captioned, larger file. Nothing is deleted by the server; clients trash what the user picks.
Trashed photos are left out. `features.similar` says the server has this.

## Delta sync

```
GET /changes?since=<cursor>&limit=500
→ {"assets": [...], "albums": [...], "cursor": 351, "more": false}
```

Returns every asset and album that changed after `since`, in the order the changes happened: new
uploads, edits, favourites, trash and restore, album changes. Deleted assets and albums come back
as tombstones: `{"id": 12, "seq": 349, "deleted": true}`. `limit` counts both together. Store `cursor` and pass it as `since` next time;
while `more` is true, fetch again straight away. `since=0` (or no `since`) is a full sync and
leaves out tombstones. `limit` is capped at 1000.

Trashed assets are part of the feed like any other (with `trashed_at` set), so a client builds
its trash view from the same data.

## Upload

Before uploading, ask which files the server already has:

```
POST /assets/check   {"sha256": ["…", "…"]}   (up to 5000)
→ {"existing": {"<sha256>": <asset id>}}
```

An asset is known by every hash it ever had: the first upload, the client's source file (see
below) and its current edit. Trashed assets count as existing. Uploads are deduplicated per user
by these hashes either way; uploading a file again returns the existing asset.

Clients that shrink photos before uploading pass `source_sha256`, the hash of the file as it is
on the device. Check with that hash before shrinking; the server then recognises the photo no
matter how it was compressed.

**Simple upload**, for photos:

```
POST /assets?filename=IMG_1234.jpg&mtime=<unix ms>&source_sha256=<hex>
X-Sha256: <hex>          optional; if the server has it (or source_sha256), the body is not read
<raw file bytes>
→ 201 {"asset": {...}, "created": true}   or   200 {"asset": {...}, "created": false}
```

**Resumable upload** ([tus 1.0](https://tus.io/protocols/resumable-upload), with the creation and
termination extensions), for videos and slow connections. Any tus client library works
(`io.tus.java.client` on Android).

```
POST /uploads
Tus-Resumable: 1.0.0
Upload-Length: 104857600
Upload-Metadata: filename <b64>,mtime <b64 unix ms>,sha256 <b64 hex>,source_sha256 <b64 hex>
→ 201  Location: /api/v1/uploads/<id>

PATCH /uploads/<id>    Upload-Offset: n   Content-Type: application/offset+octet-stream
→ 204  Upload-Offset: m
   on the final chunk also: Trunk-Asset-Id: 17, Trunk-Asset-Created: true|false

HEAD /uploads/<id>   → Upload-Offset, Upload-Length (and Trunk-Asset-Id once complete)
DELETE /uploads/<id> → 204
```

If `sha256` was given and does not match, the final PATCH returns `400` and the upload is
discarded. Unfinished uploads expire after 7 days. Files that are not photos or videos return
`415`.
