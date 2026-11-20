# Request from peek: replacing an original after an edit

peek can now edit photos that live on trunk. Today it can only upload the result as a new asset
(`<name>_edit.jpg`), because the API has no way to change an asset's file. People will want
"replace the original" too, as peek offers for photos on the phone.

## What peek would need

Keep the original: an edit should never destroy what was uploaded first. So rather than a plain
overwrite, an asset with **versions**:

- `PUT /assets/{id}/edit` (raw bytes, `X-Sha256`, like the simple upload) stores the new file as the
  asset's current version. The uploaded original is kept.
- `GET /assets/{id}/original` keeps returning what was first uploaded; the current version is
  `GET /assets/{id}/file` (or the original endpoint takes `?version=current`). Thumbnails and
  metadata (`width`, `height`, `size`, `sha256`, …) follow the current version.
- `DELETE /assets/{id}/edit` reverts to the original.
- The asset gets a field like `"edited": true` (and maybe `"edited_at"`), and the change shows up
  in `/changes` with a new `seq`, so other devices pick up the new thumbnail.
- The deduplication check should keep knowing the original's hash, so the phone does not upload
  the unedited photo again.

## Smaller things noticed while building the client

- `PATCH /assets/{id}` could accept `taken_at`/`local_time` later, for fixing wrong dates.
- A way to list the trash (`GET /assets?trashed=true`, or trashed assets in a full sync) would let
  peek show trunk's trash the way it shows the phone's.
