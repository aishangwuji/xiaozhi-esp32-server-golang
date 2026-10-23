package realtime

import (
	"context"
	"fmt"
	"strings"
)

// ProviderClient is the provider-neutral realtime transport contract used by
// the server audio bridge. Provider adapters normalize their native event
// names into Event so the device protocol does not need provider-specific
// branches.
type ProviderClient interface {
	Start(context.Context) error
	Events() <-chan Event
	SendAudio([]byte) error
	StartAudioActivity() error
	CommitAudio() error
	ClearAudio() error
	CreateResponse() error
	CancelResponse() error
	Close() error
}

func NewProviderClient(provider string, cfg Config) (ProviderClient, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", ProviderAliyunQwenRealtime:
		return NewAliyunQwenClient(cfg), nil
	default:
		return nil, fmt.Errorf("暂不支持 realtime provider: %s", provider)
	}
}

func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
