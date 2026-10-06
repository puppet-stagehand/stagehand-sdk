package local

import (
	"errors"

	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// SeedDocument writes body into collection/docID on h's Documents server
// without passing the pack-facing approval guard. It is the operator/test
// seeding path: it models the console writing on an operator's behalf. It is
// not part of the pack-facing host.Host interface, the real console exposes no
// equivalent to a pack, and production pack code must never call it. That
// confinement is pinned by TestSeedDocumentIsTestAndOperatorOnly, which fails
// if any non-test file other than this one calls it.
//
// A nil body is stored as an empty struct. The write replaces any existing
// document at that id and bumps its version, exactly like the in-process
// facets' own writes; it is not a compare-and-swap. h must come from New.
func SeedDocument(h *host.Host, collection, docID string, body map[string]any) error {
	s, ok := h.Documents.(*documentsServer)
	if !ok {
		return errors.New("SeedDocument: h.Documents is not a host.Local documents server")
	}
	st, err := structpb.NewStruct(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.putLocked(collection, docID, &hostv1.Json{Value: st}, false)
	return err
}
