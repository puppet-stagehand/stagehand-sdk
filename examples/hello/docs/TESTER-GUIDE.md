# Hello pack tester guide

## Automated checks

From the SDK repository root:

```
go test ./examples/hello ./manifest ./uibundle -race -count=1 -timeout=55s
go test ./cmd/expansion-index/... -count=1 -timeout=55s
go run ./cmd/pack-check --format json examples/hello/manifest.json
SDK_HELLO_IMAGE=1 go test ./examples/hello -run '^TestHelloImage$' -count=1 -v -timeout=10m
```

The last command requires Docker and linux/amd64 execution support. It builds
the actual production image, checks its final-layer manifest and OpenAPI,
nonroot user and executable entrypoint, then runs the inherited `/hello` binary
against the SDK fake host inside one container with external networking off.
It must print `HELLO_IMAGE_OK`. A skip is not image proof; when explicitly
requested, unavailable Docker fails the test. The harness goes only into a
temporary derivative image. Neither image is published.

The tests verify the default greeting, host-resolved identity despite forged
headers, create-only seeding that preserves an edited document, missing-asset
and undeclared-route refusals, readiness and clean shutdown. The rich manifest
in `manifest/testdata/hello-rich.json` is a tooling fixture for validator, UI and
index/listing tests; it is not this installable example.

## Manual check with a test console

1. Install and enable an appropriately reviewed and signed build on a test
   console, following the SDK feed guide. Building the example alone does not
   publish it or create a trusted signature.
2. Sign in as a viewer or higher role. Open the console's
   `/api/v1/x/hello/greeting`. Expect HTTP 200 JSON with `greeting` equal to
   `Hello` and `caller` equal to the console-resolved caller label.
3. Repeat that GET using your normal authenticated test client with
   `X-User: root` and `X-Principal-Label: root`. The verified caller still wins.
   A host request with no principal returns `unknown`, never the forged label.
4. Through your test host's Documents tooling, edit collection `greetings`,
   document `default` to `{"text":"Howdy"}`. Repeat GET: expect `Howdy`.
   Restart the worker and repeat: expect `Howdy` again. Hello has no editing
   route, configurable greeting setting or editing screen.
5. Ask the host health service for readiness after connection. Expect ready.
   Ask the Assets service for `ui.manifest.json` and `index.js`: both must
   return `NotFound`. Recheck health and GET greeting: both still work.
6. Try POST greeting and GET elsewhere through the test host transport. Expect
   a refusal, not HTTP 200. There are no slots or jobs to test.
7. Stop the worker through the host lifecycle or SIGTERM. Expect a clean stop.

The fake-host image proof does not replace this deployed-console check.
