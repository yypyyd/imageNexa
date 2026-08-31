# syntax=docker/dockerfile:1
# Frontend is open-source: built from source inside the image, then served by
# nginx (HTTP only on :2000) which also reverse-proxies the API. TLS + domain are
# handled by the user's own reverse proxy in front of this container.
# ---- build stage ----
FROM node:22-alpine AS build
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY . .
RUN npm run build

# ---- serve stage ----
FROM nginx:1.27-alpine
COPY --from=build /app/dist /usr/share/nginx/html
# Plain config (no envsubst / no ${DOMAIN}) — copied straight into conf.d.
COPY default.conf.template /etc/nginx/conf.d/default.conf
EXPOSE 2000
