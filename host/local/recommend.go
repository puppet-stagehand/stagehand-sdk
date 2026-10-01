package local

// This file implements Forge.Recommend (REC-01..03). The architectural claim
// the whole capability rests on: the grounding join lives in the host. The
// model only proposes search queries and then orders and describes REAL
// search candidates; every module fact in the response is a proto.Clone of a
// ForgeClient search result, so a module the model invents can never be
// returned (D-07).
//
// Pipeline:
//
//	validate -> resolve provider -> resolve every requested source (no LLM
//	cost on a bad request) -> LLM call #1 (extract several search queries) ->
//	one real Search per (source, query) pair -> interleaved merge, de-duplicate
//	and cap -> LLM call #2 (rank the real candidates) -> structural join.

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

const (
	// recommendMaxInputRunes bounds the caller's free text before any
	// provider resolution or network call (T-08-06).
	recommendMaxInputRunes = 2000
	// recommendDefaultMaxQueries / recommendHardMaxQueries bound how many
	// search queries the extraction reply may contribute (D-05).
	recommendDefaultMaxQueries = 3
	recommendHardMaxQueries    = 5
	// recommendMaxQueryRunes caps one extracted query.
	recommendMaxQueryRunes = 80
	// recommendSearchPageLimit is the page size of every Search call. It is
	// not overridable.
	recommendSearchPageLimit = 10
	// recommendMaxSources bounds the distinct sources one request may nominate.
	recommendMaxSources = 5
	// recommendDefaultMaxCandidates / recommendHardMaxCandidates bound the
	// merged candidate set sent to the ranker.
	recommendDefaultMaxCandidates = 20
	recommendHardMaxCandidates    = 40
	// llmMaxOutputTokensExtract / llmMaxOutputTokensRank bound each model
	// reply.
	llmMaxOutputTokensExtract = 2048
	llmMaxOutputTokensRank    = 4096

	// recommendDefaultMaxSuggestions / recommendHardMaxSuggestions bound the
	// ranked answer.
	recommendDefaultMaxSuggestions = 10
	recommendHardMaxSuggestions    = 20
	// llmCallTimeout bounds every provider call independently of the caller's
	// context. Nothing is retried.
	llmCallTimeout = 45 * time.Second
	// recommendSearchTimeout bounds the whole search fan-out (up to
	// sources x queries Search calls, each with its own enrichment requests)
	// independently of the caller's context, which may carry no deadline
	// (WR-07). Searches still outstanding when it expires are skipped.
	recommendSearchTimeout = 90 * time.Second

	// recommendMaxCandidateSummaryRunes caps one candidate's summary before it
	// enters the ranking prompt.
	recommendMaxCandidateSummaryRunes = 300
	// recommendMaxReasoningRunes caps the LLM-authored reasoning text.
	recommendMaxReasoningRunes = 400
	// recommendMaxModuleEchoRunes caps an LLM-supplied module name echoed
	// back inside a warning.
	recommendMaxModuleEchoRunes = 200
)

// Stable warning codes carried on RecommendResponse.warnings.
const (
	// recommendWarnNoResults: the search produced zero candidates; the ranker
	// was never called.
	recommendWarnNoResults = "recommend_no_results"
	// recommendWarnUnknownModuleDropped: the ranker named a module that is
	// not in the candidate set; it was dropped.
	recommendWarnUnknownModuleDropped = "recommend_unknown_module_dropped"
	// recommendWarnDuplicateDropped: the ranker repeated a module already
	// emitted; the repeat was dropped.
	recommendWarnDuplicateDropped = "recommend_duplicate_dropped"
	// recommendWarnNoRelevantModules: the ranker validly answered that none
	// of the candidates fit.
	recommendWarnNoRelevantModules = "recommend_no_relevant_modules"
	// recommendWarnCandidatesTruncated: the merged candidate set exceeded the
	// candidate budget and the tail was dropped before ranking.
	recommendWarnCandidatesTruncated = "recommend_candidates_truncated"
	// recommendWarnSearchFailed: one (source, query) search failed while at
	// least one other succeeded, so the candidate set is narrower than asked.
	recommendWarnSearchFailed = "recommend_search_failed"
)

// recommendKey is the join key: normalized, lower-cased module name plus the
// source name, so an LLM echoing "puppetlabs-apache" still joins to the
// "puppetlabs/apache" candidate.
func recommendKey(name, source string) string {
	return strings.ToLower(normalizeModuleName(name)) + "\x00" + source
}

// cleanReasoning cleans the LLM-authored reasoning string.
func cleanReasoning(s string) string { return cleanText(s, recommendMaxReasoningRunes) }

// resolveRecommendLimit resolves one per-request override: zero selects the
// default, a value above the hard maximum is refused rather than clamped (the
// forgePageParams precedent), and a negative value is invalid.
func resolveRecommendLimit(field string, requested, def, hardMax int32) (int, error) {
	switch {
	case requested == 0:
		return int(def), nil
	case requested < 0:
		return 0, status.Errorf(codes.InvalidArgument, "%s must not be negative", field)
	case requested > hardMax:
		return 0, status.Errorf(codes.InvalidArgument, "%s of %d exceeds the maximum of %d", field, requested, hardMax)
	}
	return int(requested), nil
}

// complete makes one provider call under its own timeout, independent of the
// caller's context, and never retries. The LLM client's gRPC code is
// propagated so a pack can tell a bad credential or model (FailedPrecondition)
// from a refused or truncated reply (Internal) from an outage (Unavailable),
// but the message is always the host's own: a client error can carry transport
// text and a provider can quote the prompt or the key back in an error body,
// so none of it is ever relayed.
func (s *forgeServer) complete(ctx context.Context, p LLMProvider, req LLMRequest) (string, error) {
	timeout := s.callTimeout
	if timeout <= 0 {
		timeout = llmCallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := s.llm.Complete(ctx, p, req)
	if err == nil {
		return raw, nil
	}
	switch status.Code(err) {
	case codes.FailedPrecondition:
		return "", status.Error(codes.FailedPrecondition, "the llm provider rejected the request; check the provider's API key, model and base URL")
	case codes.Internal:
		return "", status.Error(codes.Internal, "the llm provider's reply was unusable (refused, cut short or malformed)")
	default:
		return "", status.Error(codes.Unavailable, "the llm provider is unavailable, timed out or rate limited")
	}
}

// distinctSourceNames turns the requested selections into a de-duplicated,
// ordered name list. An unset selection, an empty name or an empty list all
// mean the public registry (D-06); this is pure so the source cap can be
// checked before any resolution.
func distinctSourceNames(sels []*hostv1.ForgeSourceSelection) []string {
	if len(sels) == 0 {
		return []string{publicForgeSource}
	}
	seen := make(map[string]bool, len(sels))
	names := make([]string, 0, len(sels))
	for _, sel := range sels {
		name := publicForgeSource
		if sel != nil && sel.Name != "" {
			name = sel.Name
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// resolveRecommendSources resolves every named source up front, once each, so
// an unconfigured source fails the request before any provider call.
func (s *forgeServer) resolveRecommendSources(ctx context.Context, names []string) ([]string, error) {
	resolved := make([]string, 0, len(names))
	for _, name := range names {
		_, source, err := s.resolveSource(ctx, &hostv1.ForgeSourceSelection{Name: name})
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, source)
	}
	return resolved, nil
}

// cleanQueries trims, truncates and case-insensitively de-duplicates the
// extraction reply's queries, dropping empties, and keeps at most max.
func cleanQueries(raw []string, max int) []string {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, max)
	for _, q := range raw {
		q = cleanText(q, recommendMaxQueryRunes)
		k := strings.ToLower(q)
		if q == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, q)
		if len(out) == max {
			break
		}
	}
	return out
}

// mergeInterleaved merges the per-search result lists round-robin (every
// list's first hit, then every list's second, and so on) so one query cannot
// starve the others, de-duplicating on the join key and keeping the first
// occurrence. It keeps at most max candidates and reports how many distinct
// candidates the cap dropped.
func mergeInterleaved(lists [][]*hostv1.ForgeSearchResult, max int) (kept []*hostv1.ForgeSearchResult, dropped int) {
	seen := make(map[string]bool)
	for rank := 0; ; rank++ {
		more := false
		for _, list := range lists {
			if rank >= len(list) {
				continue
			}
			more = true
			r := list[rank]
			if r == nil {
				continue
			}
			k := recommendKey(r.Name, r.Source)
			if seen[k] {
				continue
			}
			seen[k] = true
			if len(kept) < max {
				kept = append(kept, r)
			} else {
				dropped++
			}
		}
		if !more {
			return kept, dropped
		}
	}
}

// Recommend implements the Forge.Recommend RPC.
func (s *forgeServer) Recommend(ctx context.Context, req *hostv1.RecommendRequest) (*hostv1.RecommendResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "recommend request is required")
	}
	req = proto.Clone(req).(*hostv1.RecommendRequest)
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}
	if utf8.RuneCountInString(text) > recommendMaxInputRunes {
		return nil, status.Errorf(codes.InvalidArgument, "text exceeds %d characters", recommendMaxInputRunes)
	}
	if req.LlmProvider == "" {
		return nil, status.Error(codes.InvalidArgument, "llm_provider is required")
	}
	maxQueries, err := resolveRecommendLimit("max_queries", req.MaxQueries, recommendDefaultMaxQueries, recommendHardMaxQueries)
	if err != nil {
		return nil, err
	}
	maxCandidates, err := resolveRecommendLimit("max_candidates", req.MaxCandidates, recommendDefaultMaxCandidates, recommendHardMaxCandidates)
	if err != nil {
		return nil, err
	}
	maxSuggestions, err := resolveRecommendLimit("max_suggestions", req.MaxSuggestions, recommendDefaultMaxSuggestions, recommendHardMaxSuggestions)
	if err != nil {
		return nil, err
	}
	sourceNames := distinctSourceNames(req.Sources)
	if len(sourceNames) > recommendMaxSources {
		return nil, status.Errorf(codes.InvalidArgument, "sources lists %d distinct sources; at most %d are allowed", len(sourceNames), recommendMaxSources)
	}
	if s.llm == nil {
		return nil, status.Error(codes.FailedPrecondition, "no LLM client is configured for this host")
	}
	if s.client == nil {
		return nil, status.Error(codes.Unavailable, "forge client is not configured")
	}

	// Both resolutions happen before any LLM call so a bad request costs
	// nothing. An empty source list means the public registry alone (D-06):
	// Recommend never fans out to sources the caller did not nominate.
	provider, err := s.resolveLLMProvider(ctx, req.LlmProvider)
	if err != nil {
		return nil, err
	}
	sources, err := s.resolveRecommendSources(ctx, sourceNames)
	if err != nil {
		return nil, err
	}

	// LLM call #1: extraction. Only the caller's text leaves the process.
	raw, err := s.complete(ctx, provider, buildExtractionRequest(text))
	if err != nil {
		return nil, err
	}
	extracted, err := decodeExtractReply(raw)
	if err != nil {
		return nil, err
	}
	queries := cleanQueries(extracted.Queries, maxQueries)
	if len(queries) == 0 {
		return nil, status.Error(codes.Internal, "llm extraction reply held no usable search query")
	}
	resp := &hostv1.RecommendResponse{Queries: queries}

	// One real Search per (source, query) pair, sources outermost. These are
	// the only source of module facts.
	lists := make([][]*hostv1.ForgeSearchResult, 0, len(sources)*len(queries))
	var firstErr error
	searchTimeout := s.searchTimeout
	if searchTimeout <= 0 {
		searchTimeout = recommendSearchTimeout
	}
	searchCtx, cancelSearch := context.WithTimeout(ctx, searchTimeout)
	defer cancelSearch()
	budgetSpent := false
fanout:
	for _, src := range sources {
		for _, q := range queries {
			if err := ctx.Err(); err != nil {
				// The caller gave up: stop, and say so with the caller's own code.
				return nil, status.FromContextError(err).Err()
			}
			if searchCtx.Err() != nil {
				budgetSpent = true
				break fanout
			}
			searched, err := s.Search(searchCtx, &hostv1.SearchRequest{
				Query:  q,
				Source: &hostv1.ForgeSourceSelection{Name: src},
				Page:   &hostv1.Page{Limit: recommendSearchPageLimit},
			})
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				resp.Warnings = append(resp.Warnings, &hostv1.ForgeAdvisoryWarning{
					Code: recommendWarnSearchFailed,
					Message: fmt.Sprintf("a search of source %q failed (%s); its modules are missing from the candidates",
						src, status.Code(err)),
					Origins: []string{src},
				})
				continue
			}
			lists = append(lists, searched.Results)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if budgetSpent || searchCtx.Err() != nil {
		resp.Warnings = append(resp.Warnings, &hostv1.ForgeAdvisoryWarning{
			Code:    recommendWarnSearchFailed,
			Message: "the search time budget ran out; some searches were skipped and their modules are missing from the candidates",
		})
	}
	if len(lists) == 0 {
		// Every search failed: the error reaches the caller with its own code.
		if firstErr == nil {
			firstErr = status.Error(codes.Unavailable, "the search time budget ran out before any search completed")
		}
		return nil, firstErr
	}

	kept, dropped := mergeInterleaved(lists, maxCandidates)
	if dropped > 0 {
		resp.Warnings = append(resp.Warnings, &hostv1.ForgeAdvisoryWarning{
			Code: recommendWarnCandidatesTruncated,
			Message: fmt.Sprintf("the search found %d more modules than the candidate limit of %d; the rest were not ranked",
				dropped, maxCandidates),
		})
	}

	candidates := make(map[string]*hostv1.ForgeSearchResult, len(kept))
	for _, r := range kept {
		candidates[recommendKey(r.Name, r.Source)] = r
	}
	if len(candidates) == 0 {
		resp.Warnings = append(resp.Warnings, &hostv1.ForgeAdvisoryWarning{
			Code:    recommendWarnNoResults,
			Message: "the search returned no modules, so nothing was ranked",
		})
		return resp, nil
	}

	// LLM call #2: ranking. The prompt module renders the candidate metadata
	// as escaped JSON inside a labelled data block.
	rankReq, err := buildRankingRequest(text, kept)
	if err != nil {
		return nil, err
	}
	raw, err = s.complete(ctx, provider, rankReq)
	if err != nil {
		return nil, err
	}
	ranked, err := decodeRankReply(raw)
	if err != nil {
		return nil, err
	}

	// Structural join (D-07): the key must name a real candidate; rank comes
	// from output position and every module fact from the candidate.
	seen := make(map[string]bool, len(ranked.Suggestions))
	for _, entry := range ranked.Suggestions {
		if len(resp.Suggestions) == maxSuggestions {
			break
		}
		k := recommendKey(entry.Name, entry.Source)
		c, ok := candidates[k]
		switch {
		case !ok:
			resp.Warnings = append(resp.Warnings, &hostv1.ForgeAdvisoryWarning{
				Code:    recommendWarnUnknownModuleDropped,
				Message: "the ranker named a module that is not in the search results; it was dropped",
				Module:  cleanText(entry.Name, recommendMaxModuleEchoRunes),
			})
		case seen[k]:
			resp.Warnings = append(resp.Warnings, &hostv1.ForgeAdvisoryWarning{
				Code:    recommendWarnDuplicateDropped,
				Message: "the ranker named the same module more than once; the repeat was dropped",
				Module:  c.Name,
			})
		default:
			seen[k] = true
			resp.Suggestions = append(resp.Suggestions, &hostv1.RecommendedModule{
				Rank:      int32(len(resp.Suggestions) + 1),
				Reasoning: cleanReasoning(entry.Reasoning),
				Module:    proto.Clone(c).(*hostv1.ForgeSearchResult),
			})
		}
	}
	if len(ranked.Suggestions) > 0 && len(resp.Suggestions) == 0 {
		// Every key was unknown: the answer is unusable. Never fall back to
		// unranked search hits (D-09).
		return nil, status.Error(codes.Internal, "llm ranking reply named no module from the search results")
	}
	if len(ranked.Suggestions) == 0 {
		resp.Warnings = append(resp.Warnings, &hostv1.ForgeAdvisoryWarning{
			Code:    recommendWarnNoRelevantModules,
			Message: "the ranker found none of the search results relevant to the request",
		})
	}
	return resp, nil
}
