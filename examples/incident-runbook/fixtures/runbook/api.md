# Runbook: api under cache pressure

When api p99 spikes because the cache is cold or down.

1. Set `COALESCE_REQUESTS=1` on the api deployment to collapse duplicate
   upstream calls into one.
2. Roll the api pods so the flag takes effect.
3. Confirm duplicate upstream calls drop on the tracing dashboard.
4. Once cache hit rate recovers, revert the flag.
