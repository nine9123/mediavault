# idlegate

Scale-to-zero for plain Docker. A ~6 MB static binary (≈10 MB RSS with four routes)
that sits in front of your containers as a reverse proxy:

- **in use**: requests pass straight through;
- **idle for 30 min** (configurable): it `docker stop`s the containers, so they
  use **0 MB** instead of a couple of hundred;
- **next request**: it `docker start`s them, waits until the upstream answers, then
  serves that same request. The browser just sees one slow page load.

It knows nothing about any particular app, so one idlegate can guard every
container on a host. It is standard-library Go only, with no dependencies.

idlegate is its own project now: the code, tests and image build live in
[nine9123/idlegate](https://github.com/nine9123/idlegate). This directory only
keeps the notes and the compose example for running it in front of mediavault.

## Install

Every push to idlegate's `main` builds the image and pushes it to the Gitea
registry, tagged `latest` and with the first 8 characters of the commit:

```yaml
image: gitea.internal:30008/nine9/idlegate:latest
```

To build it yourself instead:

```sh
docker build -t idlegate https://github.com/nine9123/idlegate.git
# or, for a bare binary:
go install github.com/nine9123/idlegate@latest
```

## Why a proxy that stops containers, and not something else

| Approach | Why not |
|---|---|
| Middleware inside the app that "idles" it | The memory is the runtime: loaded libraries, model weights, caches, the Python/Node interpreter. A process can't give most of that back while it's still running, and every app would need its own version. Stopping the container frees all of it, for any app. |
| `docker pause` | It freezes the processes (cgroup freezer) but **keeps all of their memory**. |
| CRIU checkpoint/restore | Experimental in Docker, fragile with open sockets and mounts, and the checkpoint still sits on disk. Restarting is simpler and every app already handles it. |
| Kubernetes (Knative, KEDA HTTP add-on) | Does the same thing, if you are already on k8s. |
| [Sablier](https://github.com/sablierapp/sablier) | The established tool for this. Plugs into Traefik/Caddy/nginx and shows a "waking up" page. Worth using if you already run one of those proxies. idlegate is the same idea without needing a separate reverse proxy, and it also handles raw TCP. |

## Configure

Each route is one line:

```
LISTEN  UPSTREAM  CONTAINER[,CONTAINER...]  [key=value ...]
```

```yaml
environment:
  ROUTES: |
    :8080 http://mediavault:8080 facerec,mediavault idle=30m
    :3000 http://wiki:3000       wiki-db,wiki        health=/healthz
    :5432 tcp://postgres:5432    postgres            idle=2h
```

- Put routes on separate lines or separate them with `;`. `#` starts a comment line.
- **Containers** are Docker container names. They start in the order you list them
  and stop in reverse, so list dependencies first.
- **`http://` upstreams** are proxied per request. Long responses (video streams,
  downloads) and WebSockets count as activity until they close, so a 2-hour movie
  doesn't get stopped halfway through.
- **`tcp://` upstreams** are proxied per connection. Use this for anything that
  isn't HTTP (databases, game servers, …).

| Option | Env default | Default | Meaning |
|---|---|---|---|
| `idle=` | `IDLE_TIMEOUT` | `30m` | No traffic for this long, then stop |
| `start_timeout=` | `START_TIMEOUT` | `2m` | Give up on a wake after this long (requests get a 503) |
| `stop_grace=` | `STOP_GRACE` | `10s` | Time between SIGTERM and SIGKILL on stop |
| `health=` | – | – | After the port opens, also wait for `GET <path>` to return non-5xx (for apps that bind before they're ready) |

For a single route you can skip `ROUTES` and set `LISTEN` (default `:8080`),
`UPSTREAM`, `CONTAINERS` and `HEALTH` instead.

`DOCKER_HOST` selects the Docker API: `unix:///var/run/docker.sock` (default) or
`tcp://host:2375`.

## Wire it up

See [`docker-compose.example.yml`](docker-compose.example.yml) for mediavault. For
any service, the changes are the same:

1. **Move the published port** from the app to idlegate. The app is then reachable
   only through the gate, over the compose network.
2. **Give the app a fixed `container_name:`**. Compose's generated names
   (`<project>-<service>-1`) depend on the directory you run compose from.
3. **Use `restart: unless-stopped`** on the app, not `always`. Docker never
   restarts a container that was stopped through the API, but `always` brings it
   back on every daemon restart or reboot. With `unless-stopped`, a sleeping app
   stays asleep until someone uses it.
4. **Mount the Docker socket** into idlegate.

If your apps live in different compose projects, either attach idlegate to each
project's network (`networks: … external: true`), or have the apps publish on
`127.0.0.1:<port>` and point the upstream at the host
(`extra_hosts: ["host.docker.internal:host-gateway"]`,
`http://host.docker.internal:<port>`).

On startup, idlegate checks each route. If all of a route's containers are already
running, it treats them as just used: they get one full idle period before
sleeping. Restarting or updating idlegate never stops or starts anything.

## Things to know

- **Background work stops too.** A sleeping app doesn't run its cron jobs, periodic
  scans or queue workers. For mediavault that's fine: it rescans on wake. Don't
  put something behind the gate that has to do work while no one is using it.
- **The first request waits for the boot.** idlegate holds it until the upstream
  accepts connections (plus `health=` if set). For a slow app (more than about
  10 s), a browser just shows a long spinner. There is no "waking up" page.
- **Something that polls keeps the app awake.** An uptime monitor, a dashboard
  widget or a phone app syncing every 5 minutes all count as traffic. Point
  monitors at idlegate's port only if you want them to wake the app.
- **Stopped behind its back?** If you `docker stop` an app yourself, or it crashes,
  the next request gets a 502 and the one after that restarts it.
- **Socket access is root-equivalent.** idlegate only ever calls inspect/start/stop
  on the names you configure. If you want that enforced rather than trusted, put
  [docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy) in
  between (`CONTAINERS=1`, `POST=1`) and set `DOCKER_HOST=tcp://socket-proxy:2375`.

## Develop

Development happens in [nine9123/idlegate](https://github.com/nine9123/idlegate);
see its README for building and running the tests.
