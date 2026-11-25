---
title: Database connection health
kind: recipe
tags: [recipe, database]
---

# Database connection health

Use the `warehouse` connection. Inspect its current `schema` and `query`
operation schemas before invoking them. Pass SQL parameters separately from SQL.
Never insert credentials into this notebook or a tool argument.

Check whether the connection is read-only:

```sql read-only
SELECT current_setting('transaction_read_only') AS read_only
```

The result should be `on`. A successful connection check does not establish the
correctness or freshness of application data. Cite the execution receipt and
capture time when reporting the result.

For application questions, inspect the schema, choose the tables relevant to the
question, state the reporting window, and query only the required columns.
Distinguish missing records from a zero total. If the result is incomplete, narrow
the query or use explicit pagination before drawing a conclusion.
