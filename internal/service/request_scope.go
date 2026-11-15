package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agruntime "ragflow/internal/agent/runtime"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/permission"
)

// requestScopeFingerprintKey is an inert root key stamped into the persisted
// session DSL. It records which validated request scope the session's history
// belongs to; it is bookkeeping, never the authority for the filter itself.
const requestScopeFingerprintKey = "request_scope_fingerprint"

// ErrAgentSessionScopeMismatch rejects reusing a session whose persisted
// history was produced under a different validated retrieval scope.
var ErrAgentSessionScopeMismatch = errors.New("agent session scope changed")

// ErrRequestScopeInvalid rejects a request scope that fails preflight: filter
// targets the agent cannot reach or datasets the caller or canvas owner may
// not use.
var ErrRequestScopeInvalid = errors.New("invalid request scope")

func requestScopeFingerprint(ctx context.Context) string {
	scope := agruntime.RequestScopeFromContext(ctx)
	if scope == nil {
		return ""
	}
	return scope.Fingerprint()
}

func sessionScopeFingerprint(session *entity.API4Conversation) string {
	if session == nil || len(session.DSL) == 0 {
		return ""
	}
	fingerprint, _ := session.DSL[requestScopeFingerprintKey].(string)
	return fingerprint
}

// checkSessionScopeBoundary keeps a persisted session bound to the scope it
// was created with. A scope change requires a new session so prior answers
// and retrieval state cannot bleed across projects/phases. A fresh session
// with no rounds may still adopt the first scope its caller sends.
func checkSessionScopeBoundary(session *entity.API4Conversation, incoming string) error {
	if session == nil {
		return nil
	}
	stored := sessionScopeFingerprint(session)
	if stored == incoming {
		return nil
	}
	if incoming != "" && stored == "" && session.Round == 0 {
		return nil
	}
	return fmt.Errorf("session %s was created with a different retrieval scope; start a new session: %w", session.ID, ErrAgentSessionScopeMismatch)
}

// scopedDatasetBindings collects the dataset ids statically bound to the
// canvas: retrieval-style component params and the canvas dataset globals.
// An empty result means the agent has no fixed dataset binding, in which case
// authorization alone gates the requested scope.
func scopedDatasetBindings(dsl map[string]any) map[string]bool {
	bound := make(map[string]bool)
	addValues := func(raw any) {
		switch values := raw.(type) {
		case []any:
			for _, value := range values {
				if id, ok := value.(string); ok && strings.TrimSpace(id) != "" {
					bound[id] = true
				}
			}
		case []string:
			for _, id := range values {
				if strings.TrimSpace(id) != "" {
					bound[id] = true
				}
			}
		case string:
			if strings.TrimSpace(values) != "" {
				bound[values] = true
			}
		}
	}
	if components, ok := dsl["components"].(map[string]any); ok {
		for _, raw := range components {
			comp, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			obj, ok := comp["obj"].(map[string]any)
			if !ok {
				continue
			}
			params, ok := obj["params"].(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"dataset_ids", "kb_ids"} {
				addValues(params[key])
			}
			// Agent components carry their retrieval bindings one level
			// deeper, on each entry of params.tools.
			if tools, ok := params["tools"].([]any); ok {
				for _, rawTool := range tools {
					toolParams, ok := rawTool.(map[string]any)
					if !ok {
						continue
					}
					nested, ok := toolParams["params"].(map[string]any)
					if !ok {
						nested = toolParams
					}
					for _, key := range []string{"dataset_ids", "kb_ids"} {
						addValues(nested[key])
					}
				}
			}
		}
	}
	if globals, ok := dsl["globals"].(map[string]any); ok {
		addValues(globals["sys.dataset_id"])
		addValues(globals["sys.dataset_ids"])
	}
	return bound
}

// preflightRequestScope validates the request scope against the effective
// run DSL before the session is touched. Filter targets must be datasets the
// agent is actually bound to (when the canvas pins any) and must be usable by
// both the caller and the canvas owner — mirroring the runtime tool checks so
// a scoped request fails fast instead of degrading mid-run.
func (s *AgentService) preflightRequestScope(ctx context.Context, callerID, ownerID string, dsl map[string]any, scope *agruntime.RequestScope) error {
	if !scope.HasFilters() {
		return nil
	}
	requested := scope.DatasetIDs()
	bound := scopedDatasetBindings(dsl)
	if len(bound) > 0 {
		for _, id := range requested {
			if !bound[id] {
				return fmt.Errorf("meta_data_filter targets dataset not bound to this agent: %w", ErrRequestScopeInvalid)
			}
		}
	}
	if dao.DB == nil {
		return fmt.Errorf("request scope validation requires configured storage: %w", ErrAgentStorageError)
	}
	checker := permission.NewDatabaseChecker(dao.DB)
	for _, id := range requested {
		ref := permission.ResourceRef{Kind: permission.ResourceKindDataset, ID: id}
		if err := checker.CheckResource(ctx, permission.Subject{UserID: callerID}, ref, permission.OperationUse); err != nil {
			return fmt.Errorf("meta_data_filter dataset is not authorized for the caller: %w", ErrRequestScopeInvalid)
		}
		if ownerID != "" && ownerID != callerID {
			if err := checker.CheckResource(ctx, permission.Subject{UserID: ownerID}, ref, permission.OperationUse); err != nil {
				return fmt.Errorf("meta_data_filter dataset is not authorized for the agent owner: %w", ErrRequestScopeInvalid)
			}
		}
	}
	return nil
}
