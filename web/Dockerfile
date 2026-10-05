# syntax=docker/dockerfile:1
#
# Stage 1 builds the static site with Node; stage 2 serves it with nginx. The
# final image has no Node, no node_modules and no source: only the built files.

FROM node:24-alpine AS build
WORKDIR /app
RUN corepack enable
# Lockfile first so the dependency layer is reused until it changes.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile
COPY . .
RUN pnpm build

# The unprivileged nginx image runs as a non-root user and listens on 8080.
FROM nginxinc/nginx-unprivileged:1.29-alpine
# Files in /etc/nginx/templates are run through envsubst at start-up, which is
# how API_UPSTREAM and ISSUER_UPSTREAM get into the config.
COPY nginx.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /app/dist /usr/share/nginx/html
ENV API_UPSTREAM=http://api:8080 \
    ISSUER_UPSTREAM=http://issuer:8090
EXPOSE 8080
