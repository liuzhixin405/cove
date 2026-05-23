---
name: migrate
description: Database migration workflow: create, validate, apply
---

MIGRATION WORKFLOW:
1. Create the migration file with proper naming (YYYYMMDD_description.sql)
2. Write the up and down migrations
3. Test the migration locally on a copy of the database
4. Validate no data loss will occur
5. Apply the migration and verify schema
