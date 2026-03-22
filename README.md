# Go Local File Transfer Server

Local-network file transfer server written in Go.

## Features

- LAN-facing upload page for drag-and-drop or file picker uploads
- Localhost-only admin page for viewing, downloading, and deleting files
- SQLite metadata storage for upload origin, MIME type, size, and timestamp
- Disk-backed file storage with UUID-scoped paths to avoid collisions
- Plain HTML, CSS, and JavaScript frontend with a black-and-white theme

## Run

```bash
go mod tidy
go run .
```

Default addresses:

- Upload page: `http://<your-lan-ip>:8080`
- Admin page: `http://127.0.0.1:8081`

## Flags

```bash
go run . \
  -public-addr :8080 \
  -admin-addr 127.0.0.1:8081 \
  -upload-dir ./uploads \
  -db-path ./data/app.db
```

## Notes

- The upload server is intended for trusted local networks only.
- The admin server is bound to localhost so delete operations stay on the host machine.
- Uploaded files are streamed to disk instead of being buffered entirely in memory.