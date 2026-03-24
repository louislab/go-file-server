# Go Local File Transfer Server

Local-network file transfer server written in Go.

## Features

- LAN-facing upload page for drag-and-drop or file picker uploads
- Localhost-only admin page for viewing, downloading, and deleting files
- Live host info with direct IP routes and Bonjour hostname guidance for LAN recovery
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
- Bonjour hostname on macOS: `http://<your-hostname>.local:8080`
- Admin page: `http://127.0.0.1:8081`

## Flags

```bash
go run . \
  -public-addr :8080 \
  -admin-addr 127.0.0.1:8081 \
  -upload-dir ./uploads \
  -db-path ./data/app.db \
  -upload-session-ttl 24h \
  -upload-cleanup-interval 15m
```

## Notes

- The upload server is intended for trusted local networks only.
- The admin server is bound to localhost so delete operations stay on the host machine.
- Uploaded files are streamed to disk instead of being buffered entirely in memory.
- The admin page refreshes host network information so operators can recover after DHCP changes or router restarts.
- Frontend libraries used by the upload and admin pages are vendored under the embedded static assets, so the UI works without internet access.
- Completed resumable upload sessions are removed immediately, and incomplete sessions plus their `.part` files are cleaned up after the configured idle TTL.