# 2API administrator console

Vue 3 + Vite single-administrator control plane for the 2API gateway. There is
no public user application: `/` resolves to one-time initialization, login, or
the authenticated administrator console.

Administrator authentication uses an HttpOnly session cookie. Every backend
request sets `credentials: include`; mutations fetch `/admin/api/auth/csrf` and
send `X-CSRF-Token`. No administrator credential is stored in localStorage.
The one-time initialization form additionally sends the operator-provided
`ADMIN_BOOTSTRAP_TOKEN` only in `X-Admin-Bootstrap-Token`; it is never placed
in the JSON body or browser storage.

## Develop

```bash
npm ci
npm run dev
```

The development server listens on `http://localhost:5173` and proxies
`/admin/api`, `/health`, and `/v1` to `VITE_BACKEND` (default
`http://127.0.0.1:6666`). Keep the UI and API on the same site so the secure
session cookie and Origin protection work as intended.

## Verify and build

```bash
npm run lint:unused
npm run build
```

Production nginx serves the SPA on port 2000 and forwards the administrator
API, `/v1`, `/health/live`, and `/health/ready` to the backend container.
