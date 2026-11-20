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
  "duration_ms": 32777,
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
- Where the time comes from, in order: EXIF/QuickTime metadata, a timestamp in the file name
  (`IMG_20240131_142530`, `PXL_…`, `VID_…`, `Screenshot_…`, `signal-2024-01-31-…`), the
  client's `mtime`, upload time. For Android videos, which only store UTC, the offset is
  recovered by comparing against the local time in the file name.

| | |
|---|---|
| `GET /assets/{id}` | one asset |
| `PATCH /assets/{id}` | `{"favorite": true}` → the updated asset |
| `GET /assets/{id}/original` | the file as uploaded; supports `Range` for video seeking |
| `GET /assets/{id}/thumb/{size}` | JPEG, upright. `small`: short side 300 px; `medium`: short side 480 px; `preview`: long side 2048 px. Never upscaled. Videos get a poster frame from about 1 s in. `404` if the file cannot be rendered. Supports `If-None-Match`. |
| `POST /assets/trash` | `{"ids": [..]}` → `204` |
| `POST /assets/restore` | `{"ids": [..]}` → `204` |
| `POST /assets/delete` | `{"ids": [..]}` → `{"deleted": n}`. Only trashed assets are deleted; others are skipped. The trash is also emptied automatically after `trash_days`. |

## Delta sync

```
GET /changes?since=<cursor>&limit=500
→ {"assets": [...], "cursor": 351, "more": false}
```

Returns every asset that changed after `since`, in the order the changes happened: new uploads,
edits, favourites, trash and restore. Deleted assets come back as tombstones:
`{"id": 12, "seq": 349, "deleted": true}`. Store `cursor` and pass it as `since` next time;
while `more` is true, fetch again straight away. `since=0` (or no `since`) is a full sync and
leaves out tombstones. `limit` is capped at 1000.

## Upload

Before uploading, ask which files the server already has:

```
POST /assets/check   {"sha256": ["…", "…"]}   (up to 5000)
→ {"existing": {"<sha256>": <asset id>}}
```

Trashed assets count as existing. Uploads are deduplicated per user by SHA-256 either way;
uploading a file again returns the existing asset.

**Simple upload**, for photos:

```
POST /assets?filename=IMG_1234.jpg&mtime=<unix ms>
X-Sha256: <hex>          optional; if the server has it, the body is not read
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
Upload-Metadata: filename <b64>,mtime <b64 unix ms>,sha256 <b64 hex>
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
