# Forge Recommend: ask for modules in plain words (ELI10)

This guide is for a pack author who is new to Puppet and new to this SDK. It
explains one call, `Recommend`, from nothing to a ranked answer.

> Every module Recommend suggests came back from a real search of a real
> registry. The language model only puts those results in order and says why.
> It cannot add a module to the list.

You describe a need in one sentence, for example "I need to manage security
settings on my Windows servers". Recommend turns that into a few searches of
the Puppet Forge, shows the real results to a language model, and hands you
the model's ordering with a short reason for each pick.

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
| `max_tokens_field` | never | Only for an older OpenAI-compatible server. The name of its output-length field, if it does not understand the default `max_completion_tokens` (for example `max_tokens`). |

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
| `rank` | The host. 1 is first; it follows the model's ordering. |
| `reasoning` | The model. One or two plain sentences, cut to 400 characters. |
| `module` (name, version, source, endorsement, quality score, release date, deprecated, superseded-by, summary, tags) | The host. A copy of the real search result. |

That split is the grounding rule. The model is asked to answer with the name
and source of modules from the list it was given. The host looks each one up
in the real results and returns the **real result's** fields. A name the host
does not recognise is dropped and reported as a warning. So the model owns
only two things: the order, and the reasoning string. A module it makes up
cannot appear.

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
| `recommend_search_failed` | One search failed while another worked, so the candidates are narrower than you asked for. The `origins` field names the source. |

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
