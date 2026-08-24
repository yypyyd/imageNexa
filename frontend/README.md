# ai-gateway frontend

Vue 3 + Vite admin console for the ai-gateway backend. This replaces the old
single-file `static/admin.html`.

## Develop

```bash
npm install
npm run dev        # http://localhost:5173
```

The dev server proxies `/admin/api`, `/health`, `/images`, and `/v1` to the backend.
Start the backend separately:

```bash
cd ../backend
go run ./cmd/api   # http://127.0.0.1:6666
```

The account importer recognizes OreateAI account-export JSON. It forwards only
the Cookie and required profile metadata; exported passwords are discarded in
the browser and are never submitted to the backend.

The extended `/v1/models` catalog exposes OreateAI Seedance 2.5 durations and
the per-model image/video reference limits in both snake_case and camelCase for
downstream importers. OreateAI currently advertises no reference-audio slots.

If the backend runs elsewhere, set `VITE_BACKEND` before `npm run dev`:

```bash
VITE_BACKEND=http://192.168.1.10:6666 npm run dev
```

## Build

```bash
npm run build      # outputs static assets to ./dist
npm run preview    # serve the production build locally
```

When hosting `dist/` on a different origin than the API, set `VITE_API_BASE`
(e.g. `VITE_API_BASE=http://api-host:6666`) at build time, and add that frontend
origin to the backend's `CORS_ORIGINS` env var.
