# Plausible Analytics module for Caddy

A Caddy middleware module that tracks server-side pageviews by forwarding events to a [Plausible Analytics](https://plausible.io) instance. Works with both the hosted service and self-hosted deployments.

## How It Works

The middleware sits in the Caddy handler chain. For each request it:

1. Wraps the `ResponseWriter` to capture the HTTP status code and response `Content-Type`
2. Delegates the request to the next handler (the client receives the response immediately)
3. Fires an asynchronous pageview event to Plausible — the client is never kept waiting

Requests are silently skipped and never forwarded to Plausible when:

- The response status is 4xx or 5xx
- The requested path ends with a static asset extension (`.css`, `.js`, `.png`, `.jpg`, `.jpeg`, `.gif`, `.svg`, `.webp`, `.ico`, `.bmp`, `.tiff`, `.mp3`, `.mp4`, `.avi`, `.mov`, `.webm`, `.ogg`, `.wav`, `.flac`, `.woff`, `.woff2`, `.ttf`, `.map`)

The original visitor's `User-Agent` and IP address are forwarded to Plausible so its own bot-filtering and geo-lookup work correctly.

## Installation

### Using xcaddy

```bash
xcaddy build --with github.com/avvertix/caddy-plausible-analytics
```

### Using Docker

A sample Docker setup is included. It builds a custom Caddy image with the module baked in.

```bash
# Build and start
docker compose up --build
```

## Caddyfile Configuration

### Minimal

`plausible` is not a standard ordered directive, so register its position in the global options block:

```caddyfile
{
    order plausible before file_server
}

example.com {
    plausible {
        domain_name example.com
    }
    file_server
}
```

### With a self-hosted Plausible instance

```caddyfile
{
    order plausible before file_server
}

example.com {
    plausible {
        domain_name example.com
        base_url    https://plausible.yourhost.com
    }
    file_server
}
```

### With custom properties

```caddyfile
{
    order plausible before file_server
}

example.com {
    plausible {
        domain_name example.com
        props       content_type device_type
    }
    file_server
}
```

### Directives

| Directive | Default | Description |
|---|---|---|
| `domain_name` | — | **Required.** Site domain as configured in your Plausible dashboard |
| `base_url` | `https://plausible.io` | Root URL of the Plausible instance; override for self-hosted deployments |
| `props` | _(none)_ | Space-separated list of custom properties to attach to each event (see below) |

### Custom properties

Custom properties are opt-in. List the ones you want under `props`:

| Property | Source | Example values |
|---|---|---|
| `content_type` | MIME type of the response `Content-Type` header (charset stripped) | `text/html`, `application/json` |
| `device_type` | Coarse device class derived from `User-Agent` | `desktop`, `mobile`, `tablet`, `bot` |

## JSON Configuration

```json
{
  "handler": "plausible",
  "domain_name": "example.com",
  "base_url": "https://plausible.io",
  "props": ["content_type", "device_type"]
}
```

## Development

```bash
# Run tests
go test -v -race ./...

# Build Caddy locally with the module (requires xcaddy)
xcaddy build --with github.com/avvertix/caddy-plausible-analytics=.
```
