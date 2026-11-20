# trunk-ai

Content search for trunk: "Hund am Strand", "snow in the forest", "Geburtstagskuchen".

trunk itself runs no model; the small machines it lives on are not made for that. trunk-ai runs
where there is computing power, a desktop for instance, whenever convenient:

- it fetches the thumbnails of photos and videos that have no vector yet (the whole household's;
  it needs an admin account), computes one per picture with CLIP ViT-B/32 and sends them to trunk
  (about 70 a second on a modern CPU, no GPU needed);
- it uploads the matching text model (multilingual, quantised for phones, 135 MB) to trunk, from
  where apps download it, so they can turn search text into a vector themselves.

trunk stores the vectors and compares. Search works whenever trunk is up; photos that arrived
since the last run of trunk-ai are found by content after the next.

## Build and run

Needs the ONNX Runtime library (Arch: `pacman -S onnxruntime-cpu`, 1.29) and cgo:

```sh
cd ai && go build -o trunk-ai .
./trunk-ai index -server https://photos.example.org -user alice
./trunk-ai search -server https://photos.example.org -user alice "Schnee im Wald"
```

On the machine trunk runs on, `contrib/trunk-ai.service` and `.timer` run it hourly at low
priority (at most two cores) after new photos; instructions are in the service file.

The models (pinned by checksum) are downloaded from Hugging Face on first use into
`~/.cache/trunk-ai`; the login is remembered in `~/.config/trunk-ai`. Run `index` from a timer or
by hand; it only does what is missing.

Models: [CLIP ViT-B/32](https://huggingface.co/Xenova/clip-vit-base-patch32) (OpenAI, MIT) for
pictures, [clip-ViT-B-32-multilingual-v1](https://huggingface.co/sentence-transformers/clip-ViT-B-32-multilingual-v1)
(Apache 2.0) for text in 50+ languages.
