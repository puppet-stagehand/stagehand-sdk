// Package schema holds the agent-facing contract files for Stagehand packs.
//
// schema/proto/ is a generated, committed, verbatim copy of the canonical
// proto/ tree. CLAUDE.md, AGENTS.md, README.md and llms.txt point coding
// agents and pack authors at it, so it must never be hand-edited:
// TestSchemaProtoMatchesProto fails on any path or byte difference.
//
// Maintainer loop when the contract changes:
//
//  1. edit proto/ only
//  2. buf lint && buf generate
//  3. go generate ./schema   (copies proto/ to schema/proto/)
package schema

//go:generate go run ../cmd/schema-sync -src ../proto -dst proto
