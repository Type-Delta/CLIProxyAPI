# Embedded CPA Usage Keeper

This package contains CPA's optional in-process analytics module. It uses the
root Go module but keeps a package boundary from proxy and Management API code.

The request path may call only `Service.Observer().HandleUsage`. That adapter
copies Event v1 allowlisted values, hashes the inbound key, derives the optional
credential pseudonym, and attempts one non-blocking enqueue. Database work,
queries, pricing, and retries run outside the request goroutine.

`New` never returns a startup error. Invalid configuration or a failed backend
produces a non-nil unavailable service with typed query errors. A disabled
configuration produces a non-nil disabled service. Analytics state does not
participate in CPA readiness.

Storage implementations satisfy `Backend` and return their database-bound
32-byte identity key from `BackendFactory`. The collector is the only writer.
HTTP handlers depend on `Reader` and `Maintenance`, not SQLite types.
