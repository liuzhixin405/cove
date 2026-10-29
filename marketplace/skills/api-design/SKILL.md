---
name: api-design
description: REST API design: endpoints, schemas, OpenAPI.
---

# REST API Design

Design or review HTTP APIs that stay consistent and easy to evolve. Match the conventions the project already uses before introducing new ones.

## 1. Read what exists

- Find existing routes, error format, pagination style, auth scheme and any OpenAPI file.
- New endpoints follow those conventions even where you would choose differently.

## 2. Resources and routes

- Nouns, plural, lowercase with hyphens: `/orders`, `/orders/{orderId}/line-items`.
- Methods carry the verb: `GET` read, `POST` create, `PUT` replace, `PATCH` partial update, `DELETE` remove.
- Actions that are not CRUD become sub-resources: `POST /orders/{id}/cancel`.
- Keep nesting to one level; link further relations by id.

## 3. Requests and responses

- One field-naming style across the API (camelCase or snake_case).
- Timestamps in RFC 3339 UTC; money as integer minor units or a decimal string plus currency.
- `POST` returns `201` with the created resource and a `Location` header.
- Status codes: `400` invalid input, `401` unauthenticated, `403` forbidden, `404` missing, `409` conflict, `422` validation, `429` rate limited.
- One error shape everywhere, e.g. RFC 9457 problem details: `type`, `title`, `status`, `detail`, plus field errors.

## 4. Collections

- Paginate every list. Prefer cursor pagination (`?cursor=…&limit=…`, response `nextCursor`) for large or changing data.
- Filtering and sorting through query parameters: `?status=open&sort=-createdAt`.

## 5. Safety and evolution

- `GET`, `PUT`, `DELETE` are idempotent; accept an `Idempotency-Key` header for `POST` that creates payments or orders.
- Use `ETag`/`If-Match` where concurrent updates matter.
- Additive changes only within a version; breaking changes need a new version (`/v2` or a media-type version).

## 6. OpenAPI

Write or update the OpenAPI 3.1 document with every path, parameter, request body, response and error schema, plus examples. Validate it (e.g. `npx @redocly/cli lint openapi.yaml`) and report anything that does not match the implementation.
