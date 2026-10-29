package nlp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ragflow/internal/common"
)

const maxRetrievalCaptures = 32
const maxRetrievalCaptureBytes = 32 << 20

var retrievalCaptureMu sync.Mutex

type retrievalCandidateSnapshot struct {
	Version   int                     `json:"version"`
	Stage     string                  `json:"stage"`
	SessionID string                  `json:"session_id"`
	CreatedAt time.Time               `json:"created_at"`
	Request   retrievalCaptureRequest `json:"request"`
	Result    *RetrievalSearchResult  `json:"result"`
}

type retrievalCaptureRequest struct {
	Question               string                 `json:"question"`
	TenantIDs              []string               `json:"tenant_ids"`
	KbIDs                  []string               `json:"kb_ids"`
	DocIDs                 []string               `json:"doc_ids"`
	Page                   int                    `json:"page"`
	PageSize               int                    `json:"page_size"`
	RerankCandidatesCount  *int                   `json:"rerank_candidates_count"`
	KNNTopK                *int                   `json:"knn_top_k"`
	KNNNumCandidates       *int                   `json:"knn_num_candidates"`
	SimilarityThreshold    *float64               `json:"similarity_threshold"`
	VectorSimilarityWeight *float64               `json:"vector_similarity_weight"`
	RankFeature            *map[string]float64    `json:"rank_feature"`
	Filter                 map[string]interface{} `json:"filter"`
	VectorOnly             bool                   `json:"vector_only"`
	Language               string                 `json:"language"`
	AllowDenseFallback     *bool                  `json:"allow_dense_fallback"`
	CollapseDuplicates     *bool                  `json:"collapse_duplicates"`
	ChunkMeta              *common.ChunkMetaScope `json:"chunk_meta"`
}

type captureBuffer struct {
	bytes.Buffer
	limit int
}

func (b *captureBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("retrieval capture size limit")
	}
	return b.Buffer.Write(p)
}

func captureRetrievalCandidates(ctx context.Context, req *RetrievalRequest, result *RetrievalSearchResult) (bool, error) {
	dir := os.Getenv("RAGFLOW_RETRIEVAL_CAPTURE_DIR")
	session := os.Getenv("RAGFLOW_RETRIEVAL_CAPTURE_SESSION")
	tenant := os.Getenv("RAGFLOW_RETRIEVAL_CAPTURE_TENANT")
	if dir == "" || session == "" || tenant == "" || common.SessionIDFromContext(ctx) != session {
		return false, nil
	}
	if req == nil || len(req.TenantIDs) != 1 || req.TenantIDs[0] != tenant {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	retrievalCaptureMu.Lock()
	defer retrievalCaptureMu.Unlock()
	if !filepath.IsAbs(dir) {
		return false, errors.New("retrieval capture requires absolute directory")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return false, errors.New("retrieval capture directory unavailable")
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return false, errors.New("retrieval capture requires private directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, errors.New("retrieval capture directory unavailable")
	}
	defer root.Close()
	info, err = root.Stat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return false, errors.New("retrieval capture requires private directory")
	}
	lock, err := root.OpenFile(".capture.lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, errors.New("retrieval capture storage busy")
	}
	defer func() {
		_ = lock.Close()
		_ = root.Remove(".capture.lock")
	}()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil || len(entries) > maxRetrievalCaptures {
		return false, errors.New("retrieval capture storage limit")
	}
	snapshot := retrievalCandidateSnapshot{
		Version: 1, Stage: "post_prune_pre_score", SessionID: session, CreatedAt: time.Now().UTC(),
		Request: retrievalCaptureRequest{
			Question: req.Question, TenantIDs: req.TenantIDs, KbIDs: req.KbIDs, DocIDs: req.DocIDs,
			Page: req.Page, PageSize: req.PageSize, RerankCandidatesCount: req.RerankCandidatesCount,
			KNNTopK: req.KNNTopK, KNNNumCandidates: req.KNNNumCandidates,
			SimilarityThreshold: req.SimilarityThreshold, VectorSimilarityWeight: req.VectorSimilarityWeight,
			RankFeature: req.RankFeature, Filter: req.Filter, VectorOnly: req.VectorOnly, Language: req.Language,
			AllowDenseFallback: req.AllowDenseFallback, CollapseDuplicates: req.CollapseDuplicates, ChunkMeta: req.ChunkMeta,
		}, Result: result,
	}
	buffer := &captureBuffer{limit: maxRetrievalCaptureBytes}
	if err := json.NewEncoder(buffer).Encode(snapshot); err != nil {
		return false, errors.New("retrieval capture serialization failed")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return false, errors.New("retrieval capture identifier failed")
	}
	name := hex.EncodeToString(nonce[:]) + ".partial"
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, errors.New("retrieval capture creation failed")
	}
	defer root.Remove(name)
	_, writeErr := file.Write(buffer.Bytes())
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return false, errors.New("retrieval capture write failed")
	}
	if err := root.Link(name, hex.EncodeToString(nonce[:])+".json"); err != nil {
		return false, errors.New("retrieval capture publication failed")
	}
	return true, nil
}
