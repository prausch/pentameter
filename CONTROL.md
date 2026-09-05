# Local control surface (`POST /command`)

> **This is a local-fork feature and must not be included in any upstream PR.**
> Upstream pentameter is a read-only Prometheus exporter by explicit design
> ("No control/write surface" is a stated non-goal). This branch —
> `paul/local-control` — adds a write path for a private homelab deployment.
> Keep it off `paul/expanded-metrics`, which is the branch headed upstream.

## Why this exists

The Pentair IntelliCenter accepts only a small number of simultaneous
connections. Running pentameter (WebSocket `:6680`) alongside Homebridge's
IntelliCenter plugin (Telnet `:6681`) has repeatedly exhausted that limit and
knocked one or both offline.

The goal is to retire the Homebridge plugin and drive the pool from Hubitat
instead. If the Hubitat driver opened its *own* connection, the contention bug
would move rather than be fixed — so control reuses the WebSocket pentameter
already holds open for `RequestParamList` polling. **Adding control adds zero
connections.**

## Enabling it

The endpoint is **opt-in**. Without `PENTAMETER_COMMAND_TOKEN` set, `/command`
returns 404 and the binary behaves exactly as upstream does.

```bash
PENTAMETER_COMMAND_TOKEN=<shared-secret>
```

Requests must carry that value in an `X-Auth-Token` header. Unlike `/metrics`,
this endpoint actuates physical equipment, so it should not be as open as the
read path. The token is a LAN-scoped shared secret, not a real auth system — the
network boundary is doing most of the work.

## Requests

`POST /command`, JSON body, **exactly one action per request**.

| Action | Body | Writes |
|---|---|---|
| Switch a circuit | `{"circuit": "C0006", "state": "off"}` | circuit `STATUS` |
| Enable/disable heat | `{"body": "pool", "heat": "on"}` | body `HTSRC` |
| Set heat setpoint | `{"body": "pool", "setpoint": 82}` | body `LOTMP` |

`circuit` accepts either the objnam (`C0006`) or the friendly name pentameter
already tracks for metric labels (`Pool Light`), case-insensitively. `body`
accepts the friendly name (`Pool`), the subtype (`POOL`), or the objnam
(`B1101`).

`state` and `heat` accept `on`/`off`, `1`/`0`, `true`/`false`.

### Heat is a property of the body, not the heater

The heater object has no on/off of its own. Heat is **enabled** by assigning a
heater to a body (`HTSRC` = the heater's objnam) and **disabled** by clearing
that assignment (`HTSRC` = `00000`).

With a single installed heater you can omit the target. With more than one, the
request is rejected rather than guessed — pass `{"heater": "Solar Heater"}` (or
its objnam) to disambiguate. Picking whichever heater a map happened to iterate
first would actuate the wrong equipment.

### Setpoint bounds

Setpoints outside **40–104°F** are rejected. IntelliCenter itself accepts a
wider range than is ever sensible for a pool, and a bad value written over the
wire is a physical-equipment problem, not a display bug.

## Responses

Always a JSON object, on success and failure alike:

```json
{"status": "ok", "applied": "circuit C0006 (Pool) -> OFF"}
{"status": "error", "error": "setpoint 200.0 out of range (40-104°F)"}
```

| Status | Meaning | Client should |
|---|---|---|
| 200 | Applied | — |
| 400 | Malformed or invalid request | Fix the request; do not retry |
| 401 | Bad or missing `X-Auth-Token` | Do not retry |
| 404 | Control not enabled on this deployment | Do not retry |
| 405 | Not a POST | Do not retry |
| 503 | IntelliCenter link is down | **Retry** |

The 400/503 split matters for a polling driver: 503 is transient and worth
retrying, 400 never is.

## Examples

```bash
TOKEN=<shared-secret>
HOST=http://pentameter:8080

# Turn the pool light off
curl -sS -X POST "$HOST/command" -H "X-Auth-Token: $TOKEN" \
  -d '{"circuit": "Pool Light", "state": "off"}'

# Enable solar heat on the pool body
curl -sS -X POST "$HOST/command" -H "X-Auth-Token: $TOKEN" \
  -d '{"body": "pool", "heat": "on"}'

# Set the heat setpoint to 82°F
curl -sS -X POST "$HOST/command" -H "X-Auth-Token: $TOKEN" \
  -d '{"body": "pool", "setpoint": 82}'
```

## Confirming a command took effect

Polling runs on `PENTAMETER_INTERVAL` (60s in the homelab deployment), so a
command's effect can take up to a full interval to appear in `/metrics`. A
200 response means the IntelliCenter acknowledged the write, not that the
exporter has re-read it yet.

A client that flips a switch and immediately re-reads `/metrics` will see the
old value. Drivers should either optimistically reflect the commanded state or
wait out the interval — do not treat the stale read as a failed command.

## Concurrency note for future changes

`connMu` serializes the entire write-then-read cycle on the shared WebSocket.
The polling loop runs in its own goroutine while the HTTP server serves on
another, and every IntelliCenter call is a `WriteJSON` followed by a
messageID-correlated `ReadJSON` — without the lock, two goroutines interleave
and read each other's responses.

**Any new code path that touches `pm.conn` must take `connMu` for the whole
cycle, not just the write.** `PoolMonitor.mu` is a different lock guarding
listen mode, which does not start the HTTP server and is therefore unaffected.
