---
name: dockerize
description: Docker: Dockerfile, compose, build, push.
---

# Dockerize

Containerize the project with a small, reproducible image and verify it actually runs.

## 1. Inspect the project

- Language, version (from go.mod, package.json engines, .python-version, global.json…), build command, start command, listening port, required env vars and files.
- Reuse an existing Dockerfile or compose file if there is one; improve it rather than replacing it.

## 2. Dockerfile

- Multi-stage: a build stage with the SDK, a runtime stage with only what is needed to run.
- Pin base images to a specific version tag (not `latest`); prefer slim, distroless or alpine where the runtime allows.
- Copy dependency manifests first and install, then copy the source, so the dependency layer caches.
- Run as a non-root user; set `WORKDIR`, `EXPOSE`, and an exec-form `ENTRYPOINT`/`CMD`.
- Add a `HEALTHCHECK` when the service has a health endpoint.
- Add a `.dockerignore` (`.git`, build output, `node_modules`, local env files, secrets).

Example for Go:

```dockerfile
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/app ./cmd/app

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
```

## 3. Compose (when there are dependencies)

- One service per process; databases and caches use official images with named volumes.
- Configuration through `environment`/`env_file`; never bake secrets into the image.
- `depends_on` with `condition: service_healthy` where a health check exists.

## 4. Verify

```bash
docker build -t app:dev .
docker run --rm -p 8080:8080 app:dev
docker compose up --build
```

Hit the service (curl the health endpoint) and report the image size (`docker images app:dev`).

## 5. Push

Only when the user asks: tag with the registry and version (`docker tag app:dev registry/app:1.2.3`), then `docker push`. Confirm the registry and tag first.
