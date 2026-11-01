# Hello pack user guide

Hello is a small example for people learning to make a Puppet Stagehand
expansion pack. A pack is a helper that the console starts in a container.
This helper answers one web request. It has no screens, scheduled jobs or
settings to fill in.

With the pack installed and enabled on a test console, sign in as a viewer or
higher role and open `/api/v1/x/hello/greeting` on that console. A response for
a caller named ada looks like this:

```json
{"greeting":"Hello","caller":"ada"}
```

`caller` is the person or token the console has verified. Sending a header such
as `X-User: root` does not change it. If the host supplies no caller, the example
answers `unknown`; that does not grant extra access.

The greeting is kept in Documents, the console's storage tool. The pack needs
only `documents:rw`. On its first connection it creates a document in collection
`greetings`, ID `default`, with `{"text":"Hello"}`. If that document already
exists, it leaves it alone. A tester with access to the host Documents tool can
change `text` to `Howdy`; the next greeting says `Howdy`, including after a
worker restart. There is no document editing screen or editing route in Hello.

Build and validate this example from the SDK repository root:

```
go run ./cmd/pack-check --format json examples/hello/manifest.json
docker buildx build --load --platform linux/amd64 --provenance=false --sbom=false -f examples/hello/Dockerfile -t hello:local .
```

A standalone `docker run hello:local` has no console connection. The console
normally provides the four connection variables described in the SDK
[worker guide](../../../docs/worker.md). For a self-contained test that provides
a temporary fake host, use the [tester guide](TESTER-GUIDE.md). Building does
not sign or publish the pack. Replace the example publisher identity and follow
the SDK feed guide before submitting a pack for real installation.

Health is ready after the host connection and greeting setup complete. Missing
UI assets are refused with `NotFound`; this is expected for a pack with no
screens and does not stop the greeting.
