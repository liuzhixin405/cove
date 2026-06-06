---
name: claude-api
description: Guide implementation against Claude/OpenAI-compatible API patterns
---

# API Implementation

## Workflow
1. Identify the provider, endpoint shape, streaming mode, and auth requirements
2. Prefer existing provider abstractions in the repo
3. Validate request/response schemas with tests
4. Handle retries, rate limits, and streaming parse errors explicitly
5. Document required environment variables and configuration
