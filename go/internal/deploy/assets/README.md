`scripts/bundle-api.sh` writes the API Worker bundle here (`worker.js`, `meta.json`)
before the CLI is built, so `cloudex deploy` can upload it. The files are not
checked in.
