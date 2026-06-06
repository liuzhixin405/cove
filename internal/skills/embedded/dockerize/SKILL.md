---
name: dockerize
description: Docker: Dockerfile, compose, build, push
---

# Dockerization

## Workflow
1. Identify the application entry point and dependencies
2. Create Dockerfile with multi-stage builds where beneficial
3. Create docker-compose.yml if multiple services needed
4. Add .dockerignore to exclude unnecessary files
5. Build and test the container locally
