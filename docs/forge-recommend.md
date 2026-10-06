# Forge Recommend: ask for modules in plain words (ELI10)

This guide is for a pack author who is new to Puppet and new to this SDK. It
explains one call, `Recommend`, from nothing to a ranked answer.

> Every module Recommend suggests came back from a real search of a real
> registry. The language model only puts those results in order and says why.
> It cannot add a module to the list.

You describe a need in one sentence, for example "I need to manage security
settings on my Windows servers". Recommend turns that into a few searches of
the Puppet Forge, shows the real results to a language model, and hands you
a ranked list: the host numbers it from the order the model proposed, with
deprecated modules moved to the end, and each pick has a short reason.

## Some words first

- **Control repo** - the one repository that holds all of a site's Puppet
  code.
- **Puppetfile** - the list of modules a control repo's environment needs,
  with the version of each. Recommend never edits it.
- **Module** - a ready-made bundle of Puppet code, for example one that
  manages the Windows firewall. You add modules instead of writing the code
  yourself.
- **The Forge** - the public shop for Puppet modules
  (`forge.puppet.com`). Its source name in this SDK is `puppet-forge`. A
  company can also run a private one; each registry you can search is a
  **source**.
- **Facet** - one family of tools the console lends your pack. The Forge
  facet has three: Search, Resolve and Recommend.
- **Permission** - a line on your pack's manifest (its ID card) naming a
  facet you may use. A facet you did not list is refused.
- **Provider** - the language-model service Recommend talks to, such as
  Anthropic, OpenAI or a model server running on your own machine. You
  configure one and name it. The model is the part that reads and orders.
- **Sealed secret** - a value stored through the Secrets facet instead of in
  plain Documents. A provider's API key (its password) lives only in one.
- **Candidate** - one real search result that is shown to the model for
  ranking.
- **Grounding** - the rule that every suggestion is tied to a real search
  result. Section 4 says exactly how.
- **Warning** - a note in the answer saying something was dropped or
  narrowed. A warning is not an error; the call still succeeded.

Where Search and Resolve fit: `Search` lists modules from one source for a
keyword, and `Resolve` builds an advisory tree of what one module depends on.
Both need the `forge:rw` permission. Recommend is a third call that uses
Search for you. This guide covers Recommend only.

### What counts as the same module

A Forge module can be written two ways: `puppetlabs/stdlib` (with a slash) or
`puppetlabs-stdlib` (with a hyphen). The capital letters in the owner's name
do not matter either, so `PuppetLabs/stdlib` is the same module too. The SDK
treats every one of those spellings as one module. Before it compares two
names it folds each into a single lowercase hyphen form, such as
`puppetlabs-stdlib`. The function that does this is `code.CanonicalModuleName`
in the `code` package, so a reader can find the one place that decides it.

Two things are left alone on purpose:

- **A Git module's name is a plain name.** If a Puppetfile lists
  `mod 'my-module', :git => ...`, the hyphen is part of the name, not a
  divider between an owner and a module. It is never folded, so it is never
  mistaken for the Forge module `my/module`. The Puppetfile entry
  `mod 'my-module', '1.0.0'` (no `:git`) is a Forge entry and does match
  `my/module`.
- **What you see in a Search or Resolve result does not change.** Results
  still show the slash spelling, such as `puppetlabs/stdlib`, because that is
  the display form. The folded form is only used behind the scenes to compare.

What you can observe as a pack author:

1. A module that is already in the Puppetfile is recognised whichever way it
   was spelled. Resolve marks it `already_in_puppetfile`, even if the
   Puppetfile has an uppercase owner.
2. A write that would replace an existing entry needs approval whichever way
   the module is spelled. See
   [`docs/code-overwrite-gating.md`](code-overwrite-gating.md) for the rule.

If the Puppetfile is missing, empty or cannot be read, Resolve treats every
module as new and the call still succeeds.

## 1. Declare the two permissions

Add both of these to `permissions` in your `manifest.json`:

| Permission | What it lets you do | Why it exists |
|---|---|---|
| `forge:rw` | Search and Resolve | Reading registry data. |
| `forge:recommend` | Recommend, and only Recommend | Recommend sends your text and module details off the machine to a third-party provider, and a provider can charge money. It gets its own permission so that is a separate, visible choice. |

The two are **orthogonal**: holding one never grants the other. A pack with
only `forge:rw` that calls Recommend is refused with `PermissionDenied`
("facet not declared: forge:recommend"), and a pack with only
`forge:recommend` cannot call Search or Resolve.

You also need `secrets:rw` (to keep the provider's API key) and
`documents:rw` (for the provider's index entry, step 2). A copyable manifest
with all four is `manifest/testdata/forge-recommend.json`. Check any manifest
with the loop from `CLAUDE.md`:

```
go run ./cmd/pack-check --format json manifest/testdata/forge-recommend.json
```

When it prints `"ok": true`, the manifest is valid.

## 2. Configure a provider

A provider is set up with two calls your pack makes itself. There is no
separate Forge call for it.

**First, seal the whole configuration** with `Secrets.Store`. The value is one
JSON object:

| Key | Needed | Meaning |
|---|---|---|
| `kind` | always | `anthropic` or `openai_compatible` (see below). |
| `model` | always | The model name your provider gave you. |
| `api_key` | `anthropic`: yes. `openai_compatible`: optional | The provider's key. A model server on your own machine usually needs none. |
| `base_url` | `openai_compatible`: yes. `anthropic`: optional | Where the provider lives. For `anthropic` it defaults to `https://api.anthropic.com`. |
| `max_tokens_field` | never | Only for an older OpenAI-compatible server. The name of its output-length field, if it does not understand the default `max_completion_tokens`. Only `max_completion_tokens`, `max_tokens` and `max_output_tokens` are accepted. |

`base_url` must be `https`. Plain `http` is accepted only when the host is
`localhost` or a loopback address such as `127.0.0.1`, which is what a model
server on your own machine uses. It must not contain a username or password.
For `openai_compatible`, give the URL up to but not including
`/chat/completions`, for example `https://api.openai.com/v1` or
`http://localhost:11434/v1`.

**Second, write the index entry** with `Documents.Put` into the collection
`llm-providers`, using the provider's name as the document id. The body is
exactly three keys:

| Key | Meaning |
|---|---|
| `name` | The provider's name, the same as the document id. |
| `label` | A human-readable label. |
| `secret_ref` | The `ref` that `Secrets.Store` returned in the first call. |

Nothing else belongs in that document. In particular, the API key, the model
and the base URL do not go there; they are only in the sealed value. Putting a
key anywhere but the Secrets facet breaks this SDK's hard rule about
credentials. A `Put` with a version of zero only creates; to change an
existing entry, `Put` again with its current version.

```go
plaintext, _ := json.Marshal(map[string]any{
    "kind": "anthropic", "model": "<your model>", "api_key": "<your key>",
})
ref, err := h.Secrets.Store(ctx, &hostv1.StoreSecretRequest{
    Name: "llm-mine", Plaintext: plaintext,
})
// handle err, then:
body, _ := structpb.NewStruct(map[string]any{
    "name": "mine", "label": "My provider", "secret_ref": ref.Ref,
})
_, err = h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
    Collection: "llm-providers", DocId: "mine",
    Body: &hostv1.Json{Value: body},
})
```

Two kinds ship:

- **`anthropic`** - the Anthropic Messages API. Pick it when your provider is
  Anthropic.
- **`openai_compatible`** - the OpenAI chat-completions format. Pick it for
  OpenAI and for anything that copies that format, including local and
  self-hosted servers such as Ollama and vLLM, which you reach by setting
  `base_url`.

**Where sealed really is, in host.Local.** In `host.Local`, the in-process
host this SDK ships for testing, "sealed" means "stored through the Secrets
facet". That store keeps the value as plain bytes in a map inside the running
process. It is not encrypted at rest, and it is gone when the process ends.
The real console host implements Secrets differently. Treat `host.Local` as a
test double and do not put a production key into it.

## 3. Ask for suggestions

Call `Recommend` with a `RecommendRequest`:

| Field | Meaning |
|---|---|
| `text` | Your need in plain words. Required. At most 2000 characters. |
| `llm_provider` | The provider's name from step 2. Required. **There is no default provider:** an empty name is refused, and Recommend never picks "the only one you configured" for you. |
| `sources` | Which registries to search, as a list of names. **An empty list means the public registry (`puppet-forge`) only.** Recommend never searches a private source you did not name. A private source must itself be configured through `forge-sources` (a sealed `{base_url, auth}` plus an index entry), the same two-call shape as a provider. |
| `max_queries` | Optional. How many search terms to use. |
| `max_candidates` | Optional. How many real results to show the model. |
| `max_suggestions` | Optional. How many picks to return. |

Leave a bound at zero for the default. You may lower a bound. You may not
raise it past its maximum: asking for more is **refused** with
`InvalidArgument`, not quietly cut down. The table at the end of this
section lists every number.

```go
resp, err := h.Forge.Recommend(ctx, &hostv1.RecommendRequest{
    Text:        "I need to manage security settings on my Windows servers",
    LlmProvider: "mine",
})
```

A call makes two provider calls (first the model writes search terms, then it
ranks the real results) and runs one real search for each term in each source.
Nothing is retried.

## 4. Read the answer

`RecommendResponse` has three parts: `suggestions`, `queries` (the search terms
that were actually run) and `warnings`.

Each suggestion has:

| Field | Where it comes from |
|---|---|
| `rank` | The host. 1 is first. The host numbers the answer from the order the model proposed, with deprecated modules moved to the end. |
| `reasoning` | The model. One or two plain sentences, cut to 400 characters. |
| `module` (name, version, source, endorsement, quality score, release date, deprecated, superseded-by, summary, tags) | The host. A copy of the real search result. |

That split is the grounding rule. The model is asked to answer with the name
and source of modules from the list it was given. The host looks each one up
in the real results and returns the **real result's** fields. A name the host
does not recognise is dropped and reported as a warning. The model proposes an
order and writes the reasoning string. The host then joins each name to the real
result, moves deprecated modules below the non-deprecated ones, and numbers what
is left. A module the model makes up cannot appear, and the host never adds a
module the model did not name.

Grounding promises the module exists. It does not promise the model chose well
or that its reasoning is right. Read the reason as an opinion.

**The reasoning string is untrusted text.** The model wrote it after reading
module descriptions that anyone on the registry can edit. Show it as plain
text only. Never render it as HTML or Markdown, and never put it in a shell
command.

Warnings carry a `code`:

| Code | Meaning |
|---|---|
| `recommend_no_results` | The searches found no modules, so the model was never asked to rank. |
| `recommend_no_relevant_modules` | The model answered that none of the real results fit. |
| `recommend_unknown_module_dropped` | The model named a module that is not in the results. It was dropped. |
| `recommend_duplicate_dropped` | The model named the same module twice. The repeat was dropped. |
| `recommend_candidates_truncated` | The searches found more modules than the candidate limit; the extra ones were not ranked. |
| `recommend_search_failed` | One search failed or was skipped (the search time budget ran out) while another worked, so the candidates are narrower than you asked for. The `origins` field names the source when one search failed. |
| `recommend_superseded_module_listed` | A suggestion is marked deprecated on the registry, and the module that replaces it was also in the search results, so look at that one. The `module` field names the deprecated module. |
| `recommend_metadata_degraded` | A search worked but the registry would not give the details (summary, tags, quality score) for some modules, so they were ranked on thinner data. |

### Deprecated modules

On the Forge, a module is **deprecated** when its authors have retired it, and
usually point at a newer module that replaces it (`superseded_by`).

Recommend never hides a deprecated module and never swaps it for its
replacement, because the search really did find it. What it does is move every
deprecated module below the non-deprecated suggestions before the numbers are
handed out. So a deprecated module can never be the top pick while a live
alternative is in the same answer, and it can never take a place that a live
module would have had when you set `max_suggestions`.

The `recommend_superseded_module_listed` warning means the replacement was right
there in the search results. It is a hint to look at that module; the host does
not add it for you.

The one rule that matters when you read an answer: trust `module.deprecated` and
`module.superseded_by`. The host copied those from the registry. Never trust the
`reasoning` sentence for deprecation. The model wrote it, can be wrong, and is
now told not to talk about deprecation at all.

You can prove the ordering rule offline, with no key and no network:
`go test ./host/local -run TestRecommendDemotesDeprecatedModules -v`.

## 5. What to do next

Recommend is advice. It never writes to a Puppetfile. To add a module you like,
use the Code facet: `Code.PutPuppetfileModule`. If that module is already
listed, replacing it needs a person's approval; see
[`docs/code-overwrite-gating.md`](code-overwrite-gating.md).

## The bounds

These are fixed in code. A request may lower the three it can name, never
raise them past the maximum.

| What | Default | Maximum | Notes |
|---|---|---|---|
| Your text | - | 2000 characters | Over the limit is `InvalidArgument`. |
| Search terms (`max_queries`) | 3 | 5 | Each term is cut to 80 characters. |
| Sources (`sources`) | the public registry | 5 distinct names | More than five is `InvalidArgument`. |
| Results per search | 10 | 10 | Fixed. |
| Candidates shown to the model (`max_candidates`) | 20 | 40 | The rest are dropped with a warning. |
| Suggestions returned (`max_suggestions`) | 10 | 20 | |
| One module's summary shown to the model | 300 characters | 300 characters | Fixed. |
| Reasoning kept per suggestion | 400 characters | 400 characters | Fixed. |
| Model reply length, search terms | 2048 tokens | 2048 tokens | Fixed. |
| Model reply length, ranking | 4096 tokens | 4096 tokens | Fixed. |
| Time for one provider call | 45 seconds | 45 seconds | Fixed. No retries. |

Searches run one after another, source by source and term by term. With the
defaults and one source that is 3 searches; the most a request can ask for is
5 terms across 5 sources, which is 25 searches. The default worst case, three
terms across five sources, is 15 searches before the two provider calls. Each
search is one list request plus up to one small lookup per hit. A slow private
registry therefore adds time to every call. Whether these defaults suit a first
real run is a judgement still waiting for a human to make; see the testing
section below.

## What the errors look like

| What happened | What the caller sees |
|---|---|
| The pack does not hold `forge:recommend` | `PermissionDenied` |
| Empty or oversize text, empty provider name, a bound that is negative or over its maximum, more than 5 sources | `InvalidArgument` |
| The provider name, or a named source, was never configured | `NotFound` |
| A provider's index entry or sealed value cannot be read (no `secret_ref`, cannot reveal it, not valid JSON) | `Internal` |
| A sealed value that reads fine but cannot be used (unknown `kind`, no `model`, no key for `anthropic`, no `base_url` for `openai_compatible`, a bad base URL, a bad `max_tokens_field`) | `FailedPrecondition`, naming the provider and the field but never a value |
| The provider refused the request (bad key, bad model, wrong URL, or a redirect) | `FailedPrecondition`, "check the provider's API key, model and base URL" |
| The provider is unreachable, slow, rate limited or down | `Unavailable` |
| The provider's reply cannot be used (it declined, was cut short, was not the expected JSON, held no usable search term, or named only modules that are not in the results) | `Internal` |
| Every search failed | The first search's own error code |
| The searches found nothing | Success, no suggestions, and the `recommend_no_results` warning |
| The model said none of the results fit | Success, no suggestions, and the `recommend_no_relevant_modules` warning |

Unranked search hits are never handed back as if they were recommendations.
Error messages are written by the host; neither the provider's own error text
nor your API key is ever copied into one.

## What this does not do

- **It does not edit a Puppetfile.** Recommend only advises. Adding a module is
  `Code.PutPuppetfileModule`.
- **It is not reachable from outside this SDK this milestone.** Recommend is
  available through `host.Local` and through the proof example this milestone
  plans, which is built on `host.Local` too. It is wired into no MCP server and
  no other external tool surface. The cross-repo MCP tool is tracked as MCP-01
  in `.planning/REQUIREMENTS.md`, under v2.
- **It does not guarantee a good answer.** It guarantees real modules. A reply
  of `{}` from the ranking call, with no `suggestions` key at all, is read as a
  valid empty ranking, so it looks the same as the model deciding nothing fits.
  Requiring the key would need a code change that is not part of this phase.
- **It sends your text off the machine.** Your sentence and the module details
  of the candidates go to the provider you configured, along with the API key
  as that provider's login. Nothing else from the host is put into the prompt:
  no secrets, Documents, inventory or environment content.

Ideas considered and left for later: extra context from the caller (operating
system, Puppet version, modules already in the Puppetfile), model-written
cautions for each suggestion, a numeric relevance score, a stored default
provider, and a search that keeps widening until it has enough results.

## Scope boundary for this milestone

Recommend is reachable only through the in-process host, `host.Local`, this
milestone. It is wired into no MCP server and no other external tool surface.
The cross-repo tool that would expose it is tracked as MCP-01 in
`.planning/REQUIREMENTS.md` under v2, and waits on work in another repository.

Two commands check that claim instead of leaving it as a sentence. Run both
from the repository root, on the phase branch.

1. List the tracked files in the commands directory:

   ```
   git ls-files cmd/
   ```

   Expect exactly one line, `cmd/pack-check/main.go`. This proves no new
   program was added that could expose Recommend: the only command is the
   manifest checker.

2. Compare the module files with the main branch:

   ```
   git diff --exit-code main -- go.mod go.sum
   ```

   Expect no output and an exit status of 0. This proves no dependency was
   added or changed, so no outside MCP or vendor library came in. It says
   nothing about code you add later, and it is trivially clean if you run it on
   `main` itself.

If either check fails, a new external surface or dependency appeared. Do not
edit the check away. Treat it as a decision that needs a person.

## Testing this by hand

This is for a human with their own provider API key, because a live run
against a real provider is the one check an AI cannot do for you. You will run
a small scratch test and read what it prints. Nothing here changes the
repository; delete the scratch directory at the end.

The numbered steps say what to expect. If a step does not match, stop and
report which step and what you saw.

1. **Set up.** From the repository root, create a scratch directory:

   ```
   mkdir manualcheck
   ```

   Save this as `manualcheck/recommend_test.go`. It builds a host with the
   four permissions, does the two provider-setup calls from section 2, and
   reads your provider settings from the environment so your key never sits in
   a file:

   ```go
   package manualcheck

   import (
       "context"
       "encoding/json"
       "os"
       "strings"
       "testing"

       "google.golang.org/protobuf/types/known/structpb"

       hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
       "github.com/puppet-stagehand/stagehand-sdk/host"
       "github.com/puppet-stagehand/stagehand-sdk/host/local"
   )

   var ctx = context.Background()

   const need = "I need to manage security settings on my Windows servers"

   func newHost() *host.Host {
       return local.New([]string{"documents:rw", "secrets:rw", "forge:rw", "forge:recommend"}, "manual-check")
   }

   // configure does the two calls a pack makes: seal the whole provider
   // config, then write the name-only index document that points at it.
   func configure(t *testing.T, h *host.Host, name string, cfg map[string]any) {
       t.Helper()
       plaintext, err := json.Marshal(cfg)
       if err != nil {
           t.Fatal(err)
       }
       ref, err := h.Secrets.Store(ctx, &hostv1.StoreSecretRequest{Name: "llm-" + name, Plaintext: plaintext})
       if err != nil {
           t.Fatal(err)
       }
       body, err := structpb.NewStruct(map[string]any{"name": name, "label": name, "secret_ref": ref.Ref})
       if err != nil {
           t.Fatal(err)
       }
       if _, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
           Collection: "llm-providers", DocId: name, Body: &hostv1.Json{Value: body},
       }); err != nil {
           t.Fatal(err)
       }
   }

   // realConfig reads your provider settings from the environment.
   func realConfig(t *testing.T, key string) map[string]any {
       t.Helper()
       kind := os.Getenv("STAGEHAND_TEST_LLM_KIND")
       if kind == "" {
           t.Skip("set STAGEHAND_TEST_LLM_KIND, _MODEL, _KEY (and _BASE_URL for openai_compatible)")
       }
       cfg := map[string]any{"kind": kind, "model": os.Getenv("STAGEHAND_TEST_LLM_MODEL"), "api_key": key}
       if base := os.Getenv("STAGEHAND_TEST_LLM_BASE_URL"); base != "" {
           cfg["base_url"] = base
       }
       return cfg
   }

   func ask(t *testing.T, h *host.Host, provider string) (*hostv1.RecommendResponse, error) {
       t.Helper()
       return h.Forge.Recommend(ctx, &hostv1.RecommendRequest{Text: need, LlmProvider: provider})
   }

   // TestLive is walks 1 and 2: a real answer, then a direct registry search
   // for the lowest-ranked suggestion.
   func TestLive(t *testing.T) {
       h := newHost()
       configure(t, h, "mine", realConfig(t, os.Getenv("STAGEHAND_TEST_LLM_KEY")))
       resp, err := ask(t, h, "mine")
       if err != nil {
           t.Fatal(err)
       }
       t.Logf("searches run: %q", resp.Queries)
       for _, s := range resp.Suggestions {
           t.Logf("%d. %s %s [%s] deprecated=%t superseded-by=%q\n     reason: %s",
               s.Rank, s.Module.Name, s.Module.Version, s.Module.Source,
               s.Module.Deprecated, s.Module.SupersededBy, s.Reasoning)
       }
       for _, w := range resp.Warnings {
           t.Logf("warning %s (module %q): %s", w.Code, w.Module, w.Message)
       }
       if len(resp.Suggestions) == 0 {
           t.Fatal("no suggestions")
       }
       last := resp.Suggestions[len(resp.Suggestions)-1].Module
       short := last.Name[strings.LastIndex(last.Name, "/")+1:]
       found, err := h.Forge.Search(ctx, &hostv1.SearchRequest{Query: short})
       if err != nil {
           t.Fatal(err)
       }
       for _, r := range found.Results {
           if r.Name == last.Name {
               t.Logf("direct search found %s %s", r.Name, r.Version)
               return
           }
       }
       t.Fatalf("direct search did not find %s", last.Name)
   }

   // TestNeverConfigured is failure walk 1.
   func TestNeverConfigured(t *testing.T) {
       _, err := ask(t, newHost(), "never-configured")
       t.Logf("error: %v", err)
   }

   // TestWrongKey is failure walk 2: your real provider, a deliberately wrong key.
   func TestWrongKey(t *testing.T) {
       h := newHost()
       configure(t, h, "wrong", realConfig(t, "this-is-not-a-real-key"))
       _, err := ask(t, h, "wrong")
       t.Logf("error: %v", err)
   }

   // TestNothingListening is failure walk 3: a base URL where nothing answers.
   func TestNothingListening(t *testing.T) {
       h := newHost()
       configure(t, h, "dead", map[string]any{
           "kind": "openai_compatible", "base_url": "http://127.0.0.1:59999/v1", "model": "any",
       })
       _, err := ask(t, h, "dead")
       t.Logf("error: %v", err)
   }
   ```

   Run it with `go vet ./manualcheck`. Expect no output.

### Walk 1: a live provider run

2. **Pick your protocol kind and set the environment.** Use whichever you have
   a key for; do both if you have both. In the shell you will run the tests
   from:

   - For `anthropic`: set `STAGEHAND_TEST_LLM_KIND=anthropic`,
     `STAGEHAND_TEST_LLM_MODEL` to your model name and
     `STAGEHAND_TEST_LLM_KEY` to your key. Leave `STAGEHAND_TEST_LLM_BASE_URL`
     unset. The sealed value will be `{kind, model, api_key}`.
   - For `openai_compatible`: set `STAGEHAND_TEST_LLM_KIND=openai_compatible`,
     the model, the key, and `STAGEHAND_TEST_LLM_BASE_URL` to the URL up to
     but not including `/chat/completions` (for example
     `https://api.openai.com/v1`, or `http://localhost:11434/v1` for a local
     server, where the key may be left empty). The sealed value will be
     `{kind, base_url, model, api_key}`.

   Do not paste the key into the Go file.

3. **Run the live test:**

   ```
   go test ./manualcheck -run TestLive -v
   ```

   The input is the canonical one, about managing security settings on Windows
   servers. Expect, within about two minutes (each provider call is cut off at
   45 seconds):
   - a `searches run:` line listing one to three short search phrases;
   - several numbered suggestions, with ranks starting at 1 and counting up
     with no gaps, each a name like `author/module`, with a version and
     `[puppet-forge]`, then `deprecated=true` or `deprecated=false` and a
     `superseded-by=` value (empty unless the registry names a replacement);
   - a one-line `reason:` under each that reads as relevant to Windows
     security settings;
   - possibly some `warning` lines, which are not failures.

   A good answer has modules you can believe a Windows administrator would
   look at (for example ones about Windows policy, firewall, registry, user
   rights or hardening). The test reports `PASS`.

### Walk 2: the grounding check

4. **Read the last line of that output.** `TestLive` takes the lowest-ranked
   suggestion and searches the registry for it directly. Expect
   `direct search found <name> <version>`. Then do the same by eye: open
   `https://forge.puppet.com`, search for that module's name, and confirm it
   exists and its page matches the reason you were given. A module can fall
   outside the first page of a generic name, so if the test says it did not
   find one, look it up by hand before reporting a problem.

5. **Why the opposite cannot happen.** The model is only allowed to name
   modules from the list the host gave it, and the host returns the real search
   result for each name, not anything the model wrote. A name that is not in
   the list is dropped with a warning, and a reply that names nothing from the
   list is an `Internal` error rather than a fallback to unranked hits. This is
   proved by tests that need no key and no network. Run them:

   ```
   go test ./host/local -run 'TestRecommendGrounding|TestRecommendInvalidLLMOutput' -v
   ```

   Expect `PASS` for both.

5a. **Check deprecation from the printed flags, not from the reason.** Read
   the `deprecated=` value on every suggestion line.
   - Confirm that no line showing `deprecated=true` sits above a line showing
     `deprecated=false`. The host moves deprecated modules to the end, so this
     must hold.
   - If a `superseded-by=` name is printed, find that module on
     `https://forge.puppet.com` and confirm it exists.
   - The `reason:` text is the model's opinion and is never evidence that a
     module is deprecated. Only the printed `deprecated=` flag is. If a reason
     claims a module is deprecated while its printed flag is `false`, that is a
     prompt problem to report, not a ranking problem.
   - The ordering rule itself needs no key and no network. Prove it offline:

   ```
   go test ./host/local -run TestRecommendDemotesDeprecatedModules -v
   ```

   Expect `PASS`.

### Walk 3: a live registry check with no API key

6. **Search the real public registry.** This needs no provider and no key. It
   is skipped unless you ask for it, so continuous testing stays offline:

   ```
   STAGEHAND_LIVE_FORGE=1 go test ./host/local -run TestForgeHTTPSearchLive -v
   ```

   Expect two log lines, one for `security` and one for `hardening`, each like
   `live search "security": 20 hits, 20 with summary`, then `PASS`. A pass
   means the real registry answers, a keyword that used to overflow now
   decodes, and hits carry a summary. Without the variable the test prints
   `SKIP`, which is not a pass. If your network blocks the registry you will
   see a failure here that is not a bug in this SDK.

### Walk 4: the deliberate failures

7. **A provider name that was never configured.**

   ```
   go test ./manualcheck -run TestNeverConfigured -v
   ```

   Expect `code = NotFound` and `llm provider "never-configured" is not
   configured`. No provider was called.

8. **A deliberately wrong API key.** This uses your real provider with the key
   `this-is-not-a-real-key`, so use a hosted provider; a local server that
   needs no key will not reject it.

   ```
   go test ./manualcheck -run TestWrongKey -v
   ```

   Expect `code = FailedPrecondition` and `the llm provider rejected the
   request; check the provider's API key, model and base URL`. The wrong key
   itself must not appear anywhere in the message. If your provider answers a
   bad key with a status other than 401, 403 or 404, you may see `Internal`
   with `the llm provider's reply was unusable` instead; report which provider
   it was.

9. **A base URL with nothing listening.**

   ```
   go test ./manualcheck -run TestNothingListening -v
   ```

   Expect `code = Unavailable` and `the llm provider is unavailable, timed out
   or rate limited`, immediately. It needs no key and no network. If something
   on your machine answers on port 59999, change the port in the test.

### Walk 5: the cost note

10. **Know what a run costs.** One run makes **two provider calls**: one to
    write search terms and one to rank the results (one call if the searches
    found nothing). Each is one request and is never retried. It also runs one
    real registry search per search term per source: 3 with the defaults and
    the public registry, and up to 25 at the largest a request may ask for
    (5 terms across 5 sources). Every limit and its maximum is in the table in
    "The bounds" above. To spend less, lower `max_queries`,
    `max_candidates` and `sources` in the request.

11. **Clean up.** Delete the scratch directory with `rm -r manualcheck`, unset
    the `STAGEHAND_TEST_LLM_` variables, and confirm `git status` shows nothing
    new from you.

12. **Tell us what you assumed.** Read this whole guide once more as someone
    who has never used Puppet. Note any word, step or command it expected you
    to already know. Also say whether the default limits (3 search terms, 20
    candidates, 10 suggestions, a 45-second call) felt right for a first run.
    That judgement is still open.

If a real provider run in step 3 returns a suggestion that is not on the
registry, or a step 7 to 9 failure comes back with a different code or a
message containing your key, stop and report it: the grounding or the
redaction rule is broken.

Also stop and report it if a suggestion whose printed `deprecated=` flag is
`true` sits above one whose flag is `false`: the ordering rule is broken.
