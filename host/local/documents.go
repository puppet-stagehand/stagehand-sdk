package local

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

type documentsServer struct {
	hostv1.UnimplementedDocumentsServer
	packID string
	mu     sync.Mutex
	// collection -> doc_id -> document
	store map[string]map[string]*hostv1.Document
}

func newDocumentsServer(packID string) *documentsServer {
	return &documentsServer{packID: packID, store: map[string]map[string]*hostv1.Document{}}
}

func (s *documentsServer) Get(ctx context.Context, req *hostv1.GetDocumentRequest) (*hostv1.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	coll, ok := s.store[req.Collection]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "collection %q has no document %q", req.Collection, req.DocId)
	}
	doc, ok := coll[req.DocId]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "collection %q has no document %q", req.Collection, req.DocId)
	}
	return cloneDocument(doc), nil
}

func (s *documentsServer) Put(ctx context.Context, req *hostv1.PutDocumentRequest) (*hostv1.PutDocumentResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	coll, ok := s.store[req.Collection]
	if !ok {
		coll = map[string]*hostv1.Document{}
		s.store[req.Collection] = coll
	}
	existing, exists := coll[req.DocId]

	if req.IfVersion == 0 {
		if exists {
			return nil, status.Errorf(codes.AlreadyExists, "document %s/%s already exists (if_version=0 is create-only)", req.Collection, req.DocId)
		}
	} else {
		if !exists {
			return nil, status.Errorf(codes.FailedPrecondition, "document %s/%s does not exist (if_version=%d)", req.Collection, req.DocId, req.IfVersion)
		}
		if existing.Version != req.IfVersion {
			return nil, status.Errorf(codes.Aborted, "version mismatch on %s/%s: have %d, want %d", req.Collection, req.DocId, existing.Version, req.IfVersion)
		}
	}

	now := timestamppb.Now()
	newVersion := int64(1)
	createdAt := now
	if exists {
		newVersion = existing.Version + 1
		createdAt = existing.CreatedAt
	}
	coll[req.DocId] = &hostv1.Document{
		Collection: req.Collection, DocId: req.DocId, Body: req.Body,
		Version: newVersion, CreatedAt: createdAt, UpdatedAt: now,
	}
	return &hostv1.PutDocumentResponse{Version: newVersion}, nil
}

func (s *documentsServer) Delete(ctx context.Context, req *hostv1.DeleteDocumentRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	coll, ok := s.store[req.Collection]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "collection %q has no document %q", req.Collection, req.DocId)
	}
	existing, ok := coll[req.DocId]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "collection %q has no document %q", req.Collection, req.DocId)
	}
	if req.IfVersion != 0 && existing.Version != req.IfVersion {
		return nil, status.Errorf(codes.Aborted, "version mismatch on %s/%s: have %d, want %d", req.Collection, req.DocId, existing.Version, req.IfVersion)
	}
	delete(coll, req.DocId)
	return &emptypb.Empty{}, nil
}

func (s *documentsServer) List(ctx context.Context, req *hostv1.ListDocumentsRequest) (*hostv1.ListDocumentsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	coll := s.store[req.Collection]
	ids := make([]string, 0, len(coll))
	for id := range coll {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	start := 0
	if req.Page != nil && req.Page.Cursor != "" {
		n, err := strconv.Atoi(req.Page.Cursor)
		if err != nil || n < 0 {
			return nil, status.Errorf(codes.InvalidArgument, "invalid page cursor %q", req.Page.Cursor)
		}
		start = n
	}
	if start > len(ids) {
		start = len(ids) // an out-of-range (too large) cursor degrades to an empty page
	}
	limit := 500
	if req.Page != nil && req.Page.Limit > 0 && req.Page.Limit < 500 {
		limit = int(req.Page.Limit)
	}

	docs := make([]*hostv1.Document, 0, limit)
	end := start
	for end < len(ids) && len(docs) < limit {
		docs = append(docs, cloneDocument(coll[ids[end]]))
		end++
	}
	pageInfo := &hostv1.PageInfo{}
	if end < len(ids) {
		pageInfo.NextCursor = strconv.Itoa(end)
	}
	return &hostv1.ListDocumentsResponse{Documents: docs, Page: pageInfo}, nil
}

func (s *documentsServer) Query(ctx context.Context, req *hostv1.QueryDocumentsRequest) (*hostv1.ListDocumentsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	coll := s.store[req.Collection]
	ids := make([]string, 0, len(coll))
	for id := range coll {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var want any
	if req.Value != nil && req.Value.Value != nil {
		// callers pass Value as a Json whose Struct has one field, "v"
		// (single-field wrapper avoids inventing a scalar-JSON encoding).
		want = req.Value.Value.AsMap()["v"]
	}

	matched := make([]*hostv1.Document, 0)
	for _, id := range ids {
		doc := coll[id]
		if doc.Body == nil || doc.Body.Value == nil {
			continue
		}
		got := fieldAt(doc.Body.Value.AsMap(), req.Field)
		if matchOp(req.Op, got, want) {
			matched = append(matched, cloneDocument(doc))
		}
	}
	return &hostv1.ListDocumentsResponse{Documents: matched, Page: &hostv1.PageInfo{}}, nil
}

func cloneDocument(d *hostv1.Document) *hostv1.Document {
	return proto.Clone(d).(*hostv1.Document)
}

func fieldAt(m map[string]any, dotted string) any {
	cur := any(m)
	for _, part := range splitDots(dotted) {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = mm[part]
		if !ok {
			return nil
		}
	}
	return cur
}

func splitDots(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func matchOp(op hostv1.QueryDocumentsRequest_Op, got, want any) bool {
	switch op {
	case hostv1.QueryDocumentsRequest_EQ:
		return fmt.Sprint(got) == fmt.Sprint(want)
	case hostv1.QueryDocumentsRequest_NE:
		return fmt.Sprint(got) != fmt.Sprint(want)
	case hostv1.QueryDocumentsRequest_CONTAINS:
		gs, ok := got.(string)
		ws, ok2 := want.(string)
		return ok && ok2 && containsSubstring(gs, ws)
	case hostv1.QueryDocumentsRequest_GT, hostv1.QueryDocumentsRequest_GTE, hostv1.QueryDocumentsRequest_LT, hostv1.QueryDocumentsRequest_LTE:
		gf, ok1 := toFloat(got)
		wf, ok2 := toFloat(want)
		if !ok1 || !ok2 {
			return false
		}
		switch op {
		case hostv1.QueryDocumentsRequest_GT:
			return gf > wf
		case hostv1.QueryDocumentsRequest_GTE:
			return gf >= wf
		case hostv1.QueryDocumentsRequest_LT:
			return gf < wf
		default:
			return gf <= wf
		}
	default:
		return false
	}
}

func toFloat(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func containsSubstring(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
