package tool

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"gorm.io/gorm"
)

type scopedEmptyRetrievalService struct {
	calls   int
	request RetrievalRequest
	err     error
}

func (s *scopedEmptyRetrievalService) Search(_ context.Context, _ *gorm.DB, req RetrievalRequest) ([]RetrievalChunk, error) {
	s.calls++
	s.request = req
	return nil, s.err
}

func TestRetrievalDoesNotRelaxToolConditions(t *testing.T) {
	for _, backendError := range []error{nil, errors.New("unavailable")} {
		t.Run("backend", func(t *testing.T) {
			previous := GetRetrievalService()
			svc := &scopedEmptyRetrievalService{err: backendError}
			SetRetrievalService(svc)
			t.Cleanup(func() { SetRetrievalService(previous) })
			filter := map[string]any{"method": "manual", "conditions": []any{map[string]any{"key": "phase", "op": "=", "value": "rp"}}}
			rt := NewRetrievalToolWithDefaults(retrievalArgs{DatasetIDs: []string{"kb-1"}, MetaDataFilter: filter})
			_, err := rt.InvokableRun(t.Context(), `{"query":"test"}`)
			if (err != nil) != (backendError != nil) {
				t.Fatalf("error=%v want=%v", err, backendError)
			}
			if svc.calls != 1 || !reflect.DeepEqual(svc.request.MetaDataFilter, filter) {
				t.Fatalf("calls=%d filter=%v", svc.calls, svc.request.MetaDataFilter)
			}
		})
	}
}
