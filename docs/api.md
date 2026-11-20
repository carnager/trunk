# trunk API v1

Everything is JSON over HTTP under `/api/v1`. trunk speaks plain HTTP; put it behind a reverse
proxy (Caddy, nginx) for TLS and start it with `-trust-proxy` so login rate limiting sees real
client addresses.

Errors are `{"error": "message"}` with a fitting status code. Times are RFC 3339 in UTC. IDs are
integers.

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
