# syntax=docker/dockerfile:1
FROM rapidaai/rapida-node:22-alpine AS builder

WORKDIR /app

# Copy package files and install dependencies
COPY ui/package.json ui/yarn.lock ./
RUN --mount=type=cache,target=/root/.cache/yarn \
    yarn config set network-timeout 300000 && \
    yarn install --frozen-lockfile

# Copy all UI source files at once (rely on .dockerignore to exclude unnecessary files)
COPY ui/ ./

# Expose the port to serve the app
EXPOSE 3000

# Run React development server
CMD ["yarn", "start"]
