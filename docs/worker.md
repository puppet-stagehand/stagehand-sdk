# Writing a pack worker in Go

The `worker` package is everything between your pack's logic and the Stagehand
console. You write a normal `net/http` handler and call `worker.Run`; the
package does the connecting.

## What this is, for a ten year old

Your pack's worker is a helper that lives in its own small box (a container).
When the console starts the box, the helper **phones the console** (not the other
way round) and says "I am here". After that, two things happen over that one
phone line:

- the console asks the helper questions: "someone clicked this address, what is
  the answer?", "are you well?", "please stop";
- the helper asks the console for things it is allowed to have: "please keep this
  note for me" (the Documents tool), and so on, but only the tools written on
  your pack's ID card (`manifest.json`).

`worker.Run` is the phone. You only write the answers.

## The smallest worker

This is [`examples/hello/main.go`](../examples/hello/main.go), trimmed:

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/puppet-stagehand/stagehand-sdk/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /greeting", func(w http.ResponseWriter, r *http.Request) {
		who := "unknown"
		if p, ok := worker.PrincipalFrom(r.Context()); ok && p.GetLabel() != "" {
			who = p.GetLabel()
		}
		_, _ = w.Write([]byte("Hello, " + who))
	})

	err := worker.Run(ctx, worker.Options{
		Routes: mux,
		OnConnected: func(ctx context.Context, c *worker.Clients) error {
			// c.Documents, c.Secrets, ... are ready here. Keep what you need.
			return nil
		},
	})
	if err != nil {
		log.Printf("worker: %v", err) // Run's errors never contain secrets
		os.Exit(1)
	}
}
```

`Run` returns `nil` when the console asks the worker to stop (or your context is
cancelled) and an error for anything else: a missing or bad environment, the
console refusing the worker, an `OnConnected` error, or a lost connection. A
container manager should restart the worker on a non-zero exit.

## The environment contract

The console starts your container with four variables. You do not read them:
`worker.Run` does.

| Variable | Holds |
|----------|-------|
| `STAGEHAND_HOST_ADDR` | where to dial the console |
| `STAGEHAND_CLIENT_CERT` | the worker's client certificate, as PEM **content** (not a file path) |
| `STAGEHAND_CLIENT_KEY` | its private key, as PEM content |
| `STAGEHAND_CA_CERT` | the CA the console's certificate chains to, as PEM content |

The connection is mutual TLS with ALPN `h2`. Never print these variables, and
never put them in an error message or a log line. `worker.Run` redacts them from
its own errors; keep your code to the same standard.

## Routes and the principal rule

Routes you declare in `manifest.routes` arrive at your `http.Handler` with the
route's **relative path** as the URL path and a leading slash added: the route
`greeting` is `GET /greeting`. The Go 1.22 method-and-path patterns of
`http.ServeMux` work as written above. Anything your handler does not match is a
404 from your own mux. Every route needs an `operation_id` present in your
`/stagehand/openapi.json` (see [the image layout](image-layout.md)).

**The principal rule.** Who is calling is learned in exactly one way:

```go
p, ok := worker.PrincipalFrom(r.Context())
```

`PrincipalFrom` returns the principal the **console** resolved (a signed-in user
or a verified token). It reads only that field of the console's request. It
never reads a request header, so a browser, a script or another pack cannot
pretend to be someone by sending `X-User: root`. In your own code, do the same:
**never take identity from a header, query parameter or body field.** `ok` is
false when the console sent no principal; treat that as "unknown", not as an
administrator.

## Limits you can rely on

| What | Limit | When it is exceeded |
|------|-------|---------------------|
| Route request body | `Options.MaxBodyBytes` (default 4 MiB) | the caller gets `413` before your handler runs |
| Route response body | same cap | the caller gets `502`; the worker stops buffering at the cap |
| A handler that panics | n/a | the caller gets `500`, the panic is logged by the worker, the worker keeps running |
| A malformed request (control character or `..` in the path, empty or non-token method) | n/a | `400` before your handler runs |
| One `Assets.Get` chunk | 256 KiB sent (the console refuses over 1 MiB) | n/a, the worker chunks for you |
| UI bundle | 256 files, 8 MiB per file, 16 MiB total, `ui.manifest.json` at most 256 KiB | `pack-check --ui` reports it ([bundle contract](ui-bundle-contract.md)) |

Known limitation: the worker protocol carries each response header as one
string, so a header your handler sets more than once is joined with `, `. That
is wrong for `Set-Cookie`. A pack behind the console should not set cookies; if
you need several values, put them in the JSON body.

## Hooks

All of these are optional fields of `worker.Options`:

| Field | When it runs | Notes |
|-------|--------------|-------|
| `OnConnected(ctx, *Clients)` | once, after the console has admitted the worker | The place to keep the facet clients and seed state. An error stops `Run`. |
| `Ready(ctx) (bool, string)` | each time the console asks if the worker is healthy | `false` plus a short reason is shown to operators. A panicking hook reads as not ready. |
| `OnShutdown(ctx) error` | when the console asks the worker to stop | `Run` returns after it finishes. The worker stops even if the hook fails. |
| `OnPurge(ctx) error` | when the console asks the pack to clean up before it deletes the pack's documents | **If your pack keeps state outside the Documents facet, you must set this**, or the console is told the purge worked while your state is still there. |
| `MaxBodyBytes` | n/a | Raises or lowers the body cap above. |
| `UI` (an `fs.FS`) | n/a | Defaults to `os.DirFS("/stagehand/ui")`. A missing bundle means "this pack has no UI". |

`Clients` carries a gRPC client for every host facet (`Documents`, `Settings`,
`Secrets`, `Auth`, `Inventory`, `Code`, `Forge`). The **console** enforces the
permissions in your manifest on every call; a client existing here does not mean
you may use it. State belongs in `Documents`, secrets in `Secrets` (see
`CLAUDE.md` rules 4 and 5).

Not in this slice: the `Jobs` service in `worker.proto` is not served by
`worker.Run` yet. A `jobs` entry in your manifest does nothing until it is.

## Adding a UI bundle

`examples/hello` ships without a UI so that the example stays small, even though
its manifest still declares a `nodeDetailTab` slot and a placeholder
`ui_digest`. To give a pack screens:

1. Write `ui/src/index.tsx`, `ui/src/slots.json` and (optionally)
   `ui/src/sandbox.tsx` as described in [expansion-build.md](expansion-build.md).
2. Keep a placeholder digest in `manifest.json`
   (`"ui_digest": "sha256:" + 64 zeros`) and `slots` listing your screens.
3. Build the bundle. From the SDK repository root:

   ```
   go run ./cmd/expansion-build ui --src ui/src --out dist/ui --manifest manifest.json
   go run ./cmd/pack-check --ui dist/ui manifest.json
   ```

   `expansion-build ui` writes the real `ui_digest` into `manifest.json`.
4. In the Dockerfile, add one line **before** the manifest `COPY`:

   ```dockerfile
   COPY dist/ui/ /stagehand/ui/
   COPY manifest.json /stagehand/manifest.json
   ```

5. Leave `Options.UI` at its default. `worker.Run` serves only the files listed in
   `ui.manifest.json` (plus the manifest itself), refuses unsafe paths, and the
   console checks every byte against `ui_digest`.

If your pack has no screens, remove `slots`, `ui_digest` and any `jobs` you do
not implement from the manifest; a manifest that promises a UI the image does not
carry will not install.

## Testing with `workertest.NewFakeHost`

`worker/workertest` is the console's side of the protocol, in-process, for tests.
You get a real mTLS listener, a real worker connection and generated throwaway
certificates, with no container and no network beyond loopback.

```go
host := workertest.NewFakeHost(t, func(s grpc.ServiceRegistrar) {
	hostv1.RegisterDocumentsServer(s, myFakeDocuments)   // your fake of a facet
})
for k, v := range host.Env() {
	t.Setenv(k, v)                                       // the four STAGEHAND_* variables
}
go worker.Run(ctx, opts)                                  // your worker under test

waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
defer cancel()
_ = host.WaitConnected(waitCtx)

resp, err := host.Routes().Dispatch(ctx, &hostv1.HttpRequest{
	Method:    "GET",
	Path:      "greeting",
	Principal: &hostv1.Principal{Label: "ada"},
})
```

`host.Health()`, `host.Assets()` and `host.Lifecycle()` call the worker the same
way the console does (`Lifecycle().Shutdown` makes `Run` return `nil`), and
`host.DropWorker()` simulates a lost connection. A complete test using all of
this is [`examples/hello/main_test.go`](../examples/hello/main_test.go); it also
shows how to prove the principal rule (forged `X-User` headers lose to the
principal field).

The fake host replays the console's documented sequence; it is not the console.
Test against a real console before you publish.
