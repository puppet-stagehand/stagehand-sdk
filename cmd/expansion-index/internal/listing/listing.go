// Package listing writes the Marquee page data: the static JSON document the
// stagehand-docs site renders (src/data/schema/marquee.schema.json).
//
// RED stub: the real generator replaces this file in the next commit.
package listing

import (
	"context"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/catalogfile"
)

// PaidLabel is the only label a paid listing may carry.
const PaidLabel = "Perforce add-on, licence required"

// Document is the Marquee listing document (schema_version 1).
type Document struct {
	SchemaVersion int `json:"schema_version"`
}

// Options are the inputs of Build.
type Options struct {
	IndexRef string
	KeyPEM   []byte
	Catalog  *catalogfile.File
	Feed     string
	Remote   []remote.Option
}

// Build is a stub.
func Build(ctx context.Context, opts Options) (*Document, []catalogfile.Finding) {
	return &Document{}, nil
}

// Marshal is a stub.
func Marshal(doc *Document) ([]byte, error) { return nil, nil }
