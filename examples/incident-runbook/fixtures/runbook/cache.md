# Runbook: cache cluster recovery

Cold-restart procedure when all nodes have lost their heartbeat.

1. Announce the maintenance window in `#incident`.
2. `cache-ctl restart --all --rolling` — one node at a time, waiting for each
   to rejoin the ring before the next.
3. Watch the heartbeat dashboard until 3/3 nodes report green.
4. Do NOT flush keys — a cold cache is expected; let it warm from traffic.
