# CPA-Gateway Frontend

React 19 + Vite frontend for the CPA-Gateway panel.

## Development

```bash
npm install
npm run dev
```

Vite proxies API requests to `http://127.0.0.1:8888`.

## Build

```bash
npm run build
```

The `frontend/go.mod` file is intentional: it acts as a Go module boundary so backend-wide commands such as `go list ./...` or `go test ./...` from the repository root do not scan npm dependencies that happen to include Go source files.
