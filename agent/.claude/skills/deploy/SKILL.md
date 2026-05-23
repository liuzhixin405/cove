---
name: deploy
description: Deployment workflow: build, test, push, deploy
paths: go.mod,Dockerfile,*.yaml
allowed_tools: bash,read,write
---

DEPLOYMENT WORKFLOW:
1. Verify all tests pass with 'go test ./...'
2. Build the binary with 'go build -o bin/ .'
3. Check git status is clean and on main branch
4. Tag the release with version
5. Push to remote and deploy
