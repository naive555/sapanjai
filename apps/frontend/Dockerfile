# syntax=docker/dockerfile:1

# Declared once and used by all three stages below, so the image can't end up
# building on one Node major and running on another. Keep in sync with
# .nvmrc (local + CI) and package.json's `engines.node`.
ARG NODE_VERSION=24

########################
# Dependencies
########################
FROM node:${NODE_VERSION}-alpine AS deps
WORKDIR /app
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN corepack enable

COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile

########################
# Build
########################
FROM node:${NODE_VERSION}-alpine AS builder
WORKDIR /app
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
ENV NEXT_TELEMETRY_DISABLED=1
RUN corepack enable

COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN pnpm build

########################
# Runner
########################
FROM node:${NODE_VERSION}-alpine AS runner
WORKDIR /app

ENV NODE_ENV=production
# Next.js phones home by default. Beyond the privacy default, the outbound
# calls matter operationally: Railway's Serverless sleeps a service after ~10
# minutes with no OUTBOUND traffic, so background telemetry can keep an idle
# frontend awake and billing. Set in the builder stage too, for `next build`.
ENV NEXT_TELEMETRY_DISABLED=1
# Without an explicit HOSTNAME, the standalone server binds to the
# container's assigned network IP instead of all interfaces — external
# port-forwarding still works (Docker routes to whatever's listening), but
# a loopback-based HEALTHCHECK (127.0.0.1) then always fails with
# "connection refused" since nothing listens there. A well-known Next.js
# standalone + Docker gotcha.
ENV HOSTNAME=0.0.0.0
RUN addgroup -S app && adduser -S app -G app

COPY --from=builder /app/public ./public
COPY --from=builder --chown=app:app /app/.next/standalone ./
COPY --from=builder --chown=app:app /app/.next/static ./.next/static

USER app

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:3000 || exit 1

CMD ["node", "server.js"]
