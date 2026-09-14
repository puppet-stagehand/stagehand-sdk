package local

import (
	"context"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// settingsServer is the in-memory Settings facet: a single SettingsDocument
// per host.Local instance, always available regardless of declared
// permissions (see local.New).
type settingsServer struct {
	hostv1.UnimplementedSettingsServer
	doc *hostv1.SettingsDocument
}

func newSettingsServer() *settingsServer {
	return &settingsServer{doc: &hostv1.SettingsDocument{Values: &hostv1.Json{Value: &structpb.Struct{}}, Version: 0}}
}

func (s *settingsServer) Current(ctx context.Context, _ *emptypb.Empty) (*hostv1.SettingsDocument, error) {
	return proto.Clone(s.doc).(*hostv1.SettingsDocument), nil
}

// Subscribe sends the current settings once, then blocks until the stream's
// context is cancelled. host.Local has no operator UI to push a change
// from, so there is nothing further to stream in this slice — a real host
// implementation pushes a SettingsChange on every operator edit.
func (s *settingsServer) Subscribe(_ *emptypb.Empty, stream hostv1.Settings_SubscribeServer) error {
	if err := stream.Send(&hostv1.SettingsChange{Version: s.doc.Version}); err != nil {
		return err
	}
	<-stream.Context().Done() // context cancellation is the normal way a Subscribe call ends
	return nil
}
