# trunk-ai

Content search and faces for trunk.

trunk only stores and compares vectors. trunk-ai makes them:

- **Content**: CLIP vectors for every photo and video, about 70 per second on a desktop CPU. No
  GPU needed.
- **Faces**: finds faces and computes a vector for each. trunk groups them into people, which you
  then name in the app.
- **Text model**: uploads a small multilingual model (135 MB) to trunk. The app downloads it
  from there and turns search text into a vector on the phone.

New photos become searchable by content after the next run.

## Build and run

You need cgo and ONNX Runtime (Arch: `onnxruntime-cpu`).

```sh
go build -o trunk-ai .
./trunk-ai index -server https://photos.example.org -user alice
./trunk-ai search -server https://photos.example.org -user alice "snow in the forest"
```

It needs an admin account, because it indexes the whole household. It asks for the password
once and remembers the login in `~/.config/trunk-ai`. The models are downloaded on first use to
`~/.cache/trunk-ai`, about 1 GB, pinned by checksum. `index` only does what's missing.

To run it next to trunk: `contrib/trunk-ai.service` and `.timer` run it hourly, at low priority
and on at most two cores. Setup steps are in the service file.

## Models

- [CLIP ViT-B/32](https://huggingface.co/Xenova/clip-vit-base-patch32) (MIT) for pictures
- [clip-ViT-B-32-multilingual-v1](https://huggingface.co/sentence-transformers/clip-ViT-B-32-multilingual-v1) (Apache 2.0) for search text
- [YuNet](https://huggingface.co/opencv/face_detection_yunet) (MIT) and [SFace](https://huggingface.co/opencv/face_recognition_sface) (Apache 2.0) for faces

InsightFace would be better at faces, but its models are licensed for non-commercial use only.
