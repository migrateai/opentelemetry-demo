# Copyright The OpenTelemetry Authors
# SPDX-License-Identifier: Apache-2.0

FROM node:22-alpine AS builder
RUN apk add --no-cache libc6-compat

WORKDIR /app

COPY ./src/frontend/package*.json ./
RUN npm clean-install --no-audit --no-progress
COPY ./src/frontend/protos ./protos
COPY ./src/frontend .

# Create .next directory with proper permissions
RUN mkdir -p .next && chown -R node:node /app

USER node

RUN npm run build

FROM node:22-alpine AS deps

USER node

WORKDIR /app
COPY --chown=node:node --from=builder /home/node/.npm/ /home/node/.npm/
COPY ./src/frontend/package*.json ./
RUN NODE_ENV=production npm clean-install --prefer-offline --no-audit --no-progress


FROM node:22-alpine AS runner
WORKDIR /app

ENV NODE_ENV=production

RUN addgroup -g 1001 -S nodejs
RUN adduser -S nextjs -u 1001

COPY --from=builder /app/next.config.js ./
COPY --from=builder /app/utils/telemetry/Instrumentation.js ./
COPY --from=builder /app/public ./public
COPY --from=builder /app/package.json ./package.json
COPY --from=deps /app/node_modules ./node_modules

COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static

USER nextjs

ENV PORT=8080
EXPOSE ${PORT}

ENTRYPOINT ["npm", "start"]

