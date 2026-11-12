package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ragflow/internal/agent/runtime"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/permission"
	permissionresponse "ragflow/internal/permission/response"

	"gorm.io/gorm"
)

type scopedDocument struct {
	datasetID    string
	documentID   string
	documentName string
}

type scopedDocumentChecker interface {
	checkDocumentScope(context.Context, *gorm.DB, string, string) (scopedDocument, error)
}

func authorizeRetrievalDatasets(ctx context.Context, db *gorm.DB, datasetIDs []string) error {
	userID := ""
	if state, err := runtime.GetStateFromContext(ctx); err == nil && state != nil {
		userID, _ = state.Sys["user_id"].(string)
	}
	if err := CheckCanvasDependencies(ctx, db, permission.ResourceKindDataset, datasetIDs); err != nil {
		_, permissionErr := permissionresponse.Normalize(err)
		return permissionErr
	}
	checker := permission.NewDatabaseChecker(db)
	for _, id := range datasetIDs {
		if err := checker.CheckResource(ctx, permission.Subject{UserID: strings.TrimSpace(userID)}, permission.ResourceRef{Kind: permission.ResourceKindDataset, ID: id}, permission.OperationUse); err != nil {
			_, permissionErr := permissionresponse.Normalize(err)
			return permissionErr
		}
	}
	return nil
}

func (a *NLPRetrievalAdapter) checkDocumentScope(ctx context.Context, db *gorm.DB, documentID, datasetID string) (scopedDocument, error) {
	scope := runtime.RequestScopeFromContext(ctx)
	if a == nil || db == nil || !scope.HasFilters() {
		return scopedDocument{}, fmt.Errorf("document scope checker is unavailable")
	}
	doc, err := dao.NewDocumentDAO().GetByID(ctx, db, documentID)
	if err != nil || doc == nil || doc.Status == nil || *doc.Status != string(entity.StatusValid) {
		return scopedDocument{}, fmt.Errorf("document is unavailable for scoped tools")
	}
	if err := authorizeRetrievalDatasets(ctx, db, []string{doc.KbID}); err != nil {
		return scopedDocument{}, err
	}
	applies, err := scope.AppliesTo([]string{doc.KbID})
	if err != nil || !applies || (datasetID != "" && datasetID != doc.KbID) {
		return scopedDocument{}, fmt.Errorf("document is outside the selected dataset scope")
	}
	ids, err := a.resolveHardScope(ctx, scope.Filter(), []string{doc.ID}, []string{doc.KbID}, "request document")
	if err != nil {
		return scopedDocument{}, err
	}
	for _, id := range ids {
		if id == doc.ID {
			name := ""
			if doc.Name != nil {
				name = *doc.Name
			}
			return scopedDocument{datasetID: doc.KbID, documentID: doc.ID, documentName: name}, nil
		}
	}
	return scopedDocument{}, fmt.Errorf("document does not match the explicit request filters")
}

func authorizeScopedMCP(ctx context.Context, name string, raw json.RawMessage) (json.RawMessage, error) {
	if !runtime.RequestScopeFromContext(ctx).HasFilters() {
		return raw, nil
	}
	switch name {
	case "document_search", "document_read", "file_sheets", "file_preview", "file_query":
	default:
		return nil, fmt.Errorf("MCP tool %q does not support explicit request scope", name)
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return nil, fmt.Errorf("invalid scoped document tool arguments")
	}
	docID, ok := args["document_id"].(string)
	if !ok || strings.TrimSpace(docID) == "" {
		return nil, fmt.Errorf("scoped document tools require document_id from a scoped retrieval result")
	}
	datasetID := ""
	if value, exists := args["dataset_id"]; exists {
		datasetID, ok = value.(string)
		if !ok {
			return nil, fmt.Errorf("scoped document tools require a string dataset_id")
		}
	}
	checker, ok := GetRetrievalService().(scopedDocumentChecker)
	if !ok {
		return nil, fmt.Errorf("document scope checker is not configured")
	}
	trusted, err := checker.checkDocumentScope(ctx, dao.DB, docID, datasetID)
	if err != nil {
		return nil, err
	}
	if trusted.datasetID == "" || trusted.documentID == "" {
		return nil, fmt.Errorf("document scope checker returned no document identity")
	}
	args["document_id"] = trusted.documentID
	args["dataset_id"] = trusted.datasetID
	delete(args, "document_name")
	if trusted.documentName != "" {
		args["document_name"] = trusted.documentName
	}
	return json.Marshal(args)
}

func (r *RetrievalTool) validateRequestScopeBinding(ctx context.Context, args retrievalArgs) error {
	if !runtime.RequestScopeFromContext(ctx).HasFilters() {
		return nil
	}
	if args.RetrievalFrom != "dataset" || r.defaults.RetrievalFrom != "dataset" {
		return fmt.Errorf("explicit request scope supports configured dataset retrieval only")
	}
	configured := compactStrings(r.defaults.DatasetIDs)
	requested := compactStrings(args.DatasetIDs)
	if len(configured) == 0 {
		return fmt.Errorf("explicit request scope requires fixed configured datasets")
	}
	allowed := make(map[string]bool, len(configured))
	for _, id := range configured {
		if strings.Contains(id, "@") {
			return fmt.Errorf("explicit request scope requires fixed configured datasets")
		}
		allowed[id] = true
	}
	if len(requested) != len(configured) {
		return fmt.Errorf("tool arguments cannot change request-scoped dataset bindings")
	}
	for _, id := range requested {
		if !allowed[id] {
			return fmt.Errorf("tool arguments cannot change request-scoped dataset bindings")
		}
	}
	return nil
}
