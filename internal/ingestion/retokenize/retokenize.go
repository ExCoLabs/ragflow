//
//  Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.
//

// Package retokenize recomputes the full-text token fields of chunks that are
// already indexed.
//
// Re-parsing a dataset after a tokenizer change (for example switching it to a
// language the tokenizer handles differently) redoes parsing, chunking and
// embedding although only the token fields changed. This package rewrites just
// those fields in the document store: content_ltks, content_sm_ltks,
// title_tks, title_sm_tks, important_tks and question_tks. Text, vectors,
// positions and metadata are left untouched, and the dataset stays searchable
// while it runs.
//
// The fields are derived the way the ingestion Tokenizer component derives
// them (component.TokenizeContent / TokenizeTitle), with the dataset's
// language. Compiled artifacts that share the chunk index (knowledge graph,
// wiki and other compile_kwd rows) build their token fields differently and
// are skipped. A chunk whose content tokens were built from an extractor
// summary gets them rebuilt from its stored text, since the summary is not
// persisted.
//
// The work is driven by a scan of the chunk index, not by the document table:
// a chunk names its dataset in kb_id, so the run covers every chunk of the
// dataset whatever the document rows say. Options.Slices splits that scan
// across processes; slices are disjoint, so N processes with slices 0..N-1
// cover the dataset once.
//
// Only chunks whose token fields differ are written, which makes a run
// idempotent: an interrupted one is finished by starting it again, and a dry
// run that reports zero chunks to update proves a dataset is converted.
// Elasticsearch only.
package retokenize

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"ragflow/internal/dao"
	"ragflow/internal/engine"
	"ragflow/internal/engine/elasticsearch"
	"ragflow/internal/ingestion/component"
	"ragflow/internal/tokenizer"
)

// TokenFields are the chunk fields a run may rewrite.
var TokenFields = []string{"content_ltks", "content_sm_ltks", "title_tks", "title_sm_tks", "important_tks", "question_tks"}

// sourceFields are read back with every chunk: the inputs of the token fields,
// and the token fields themselves so unchanged chunks can be skipped.
var sourceFields = append([]string{"content_with_weight", "docnm_kwd", "important_kwd", "question_kwd"}, TokenFields...)

// compiledArtifactFields mark rows of the chunk index that are not document
// chunks: knowledge-graph entities and relations, wiki pages and the other
// knowledge-compiler outputs.
var compiledArtifactFields = []string{"compile_kwd", "type_kwd", "knowledge_graph_kwd"}

// Options configures a run.
type Options struct {
	DatasetIDs []string
	// Language overrides the dataset's language.
	Language string
	// DryRun computes and reports, and writes nothing.
	DryRun bool
	// Slice and Slices split the scan across processes; see the package doc.
	Slice  int
	Slices int
	// BatchSize is the number of chunks per bulk update request.
	BatchSize int
	// ScanSize is the number of chunks per scroll page.
	ScanSize int
}

type datasetIDs []string

func (d *datasetIDs) String() string { return strings.Join(*d, ",") }

func (d *datasetIDs) Set(value string) error {
	*d = append(*d, value)
	return nil
}

// ParseFlags parses the retokenize command line.
func ParseFlags(name string, args []string, output io.Writer) (*Options, error) {
	opts := &Options{}
	var ids datasetIDs
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Var(&ids, "kb-id", "dataset id (repeatable)")
	fs.StringVar(&opts.Language, "language", "", "tokenizer language; defaults to the dataset's language")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "compute and report, write nothing")
	fs.IntVar(&opts.Slice, "slice", 0, "which slice of the scan this process takes (0-based)")
	fs.IntVar(&opts.Slices, "slices", 1, "how many processes share the scan; each takes one --slice")
	fs.IntVar(&opts.BatchSize, "batch-size", 500, "chunks per bulk update request")
	fs.IntVar(&opts.ScanSize, "scan-size", 500, "chunks per scan request")
	fs.Usage = func() {
		fmt.Fprintf(output, "Usage: %s --kb-id <dataset id> [--kb-id ...] [OPTIONS]\n\n", name)
		fmt.Fprintf(output, "Recompute the full-text token fields of a dataset's indexed chunks without\n")
		fmt.Fprintf(output, "re-parsing or re-embedding them. Elasticsearch only.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument: %s", fs.Arg(0))
	}
	opts.DatasetIDs = ids
	if len(opts.DatasetIDs) == 0 {
		return nil, errors.New("--kb-id is required")
	}
	if opts.Slices < 1 || opts.Slice < 0 || opts.Slice >= opts.Slices {
		return nil, fmt.Errorf("--slice must be in [0, %d) for --slices %d", opts.Slices, opts.Slices)
	}
	if opts.BatchSize < 1 || opts.ScanSize < 1 {
		return nil, errors.New("--batch-size and --scan-size must be positive")
	}
	return opts, nil
}

// ChunkFields returns the token fields of one chunk, computed from its stored
// inputs the way ingestion computes them. Only fields whose input exists on
// the chunk are returned; a chunk without keywords keeps whatever
// important_tks it had.
func ChunkFields(tok tokenizer.Tokenizer, source map[string]interface{}) (map[string]string, error) {
	fields := make(map[string]string, len(TokenFields))
	if content, ok := source["content_with_weight"].(string); ok && strings.TrimSpace(content) != "" {
		ltks, smLtks, err := component.TokenizeContent(tok, content)
		if err != nil {
			return nil, fmt.Errorf("content: %w", err)
		}
		fields["content_ltks"], fields["content_sm_ltks"] = ltks, smLtks
	}
	if name, ok := source["docnm_kwd"].(string); ok && strings.TrimSpace(name) != "" {
		tks, smTks, err := component.TokenizeTitle(tok, component.TitleStem(name))
		if err != nil {
			return nil, err
		}
		fields["title_tks"], fields["title_sm_tks"] = tks, smTks
	}
	// The Extractor tokenizes the keywords it stores joined by spaces and the
	// questions joined by newlines.
	if keywords := stringList(source["important_kwd"]); len(keywords) > 0 {
		tks, err := tok.Tokenize(strings.Join(keywords, " "))
		if err != nil {
			return nil, fmt.Errorf("keywords: %w", err)
		}
		fields["important_tks"] = tks
	}
	if questions := stringList(source["question_kwd"]); len(questions) > 0 {
		tks, err := tok.Tokenize(strings.Join(questions, "\n"))
		if err != nil {
			return nil, fmt.Errorf("questions: %w", err)
		}
		fields["question_tks"] = tks
	}
	return fields, nil
}

func stringList(value interface{}) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case string:
		// A single-valued keyword field may come back as a bare string.
		if v != "" {
			return []string{v}
		}
	}
	return nil
}

// ChangedFields is the subset of fields that differs from what the chunk holds
// now, or nil when the chunk is up to date.
func ChangedFields(source map[string]interface{}, fields map[string]string) map[string]interface{} {
	var changed map[string]interface{}
	for k, v := range fields {
		if current, ok := source[k].(string); ok && current == v {
			continue
		}
		if changed == nil {
			changed = make(map[string]interface{}, len(fields))
		}
		changed[k] = v
	}
	return changed
}

// ChunkStore is the part of the Elasticsearch engine a run uses.
type ChunkStore interface {
	ScanChunks(ctx context.Context, indexName, datasetID string, opts elasticsearch.ScanOptions, fn func(id string, source map[string]interface{}) error) error
	UpdateChunkFields(ctx context.Context, indexName string, updates []elasticsearch.ChunkFieldUpdate) (int, []string, error)
}

// Dataset is what a run needs to know about the dataset it converts.
type Dataset struct {
	ID       string
	Name     string
	TenantID string
	Language string
}

// Result counts what a run did. Updated counts the chunks that would be
// written on a dry run.
type Result struct {
	Scanned int
	Updated int
	Failed  int
}

// progressEvery is how often (in scanned chunks) a run reports progress.
const progressEvery = 100000

// RunDataset rewrites the token fields of one slice of a dataset's chunks.
func RunDataset(ctx context.Context, store ChunkStore, ds Dataset, opts Options, out io.Writer) (Result, error) {
	language := opts.Language
	if language == "" {
		language = ds.Language
	}
	if language == "" {
		language = "English"
	}
	slices := max(opts.Slices, 1)
	indexName := fmt.Sprintf("ragflow_%s", ds.TenantID)
	fmt.Fprintf(out, "retokenize: dataset %s (%s) language=%s index=%s slice=%d/%d\n", ds.Name, ds.ID, language, indexName, opts.Slice, slices)

	tok := tokenizer.New(language)
	var res Result
	var batch []elasticsearch.ChunkFieldUpdate
	started := time.Now()

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		defer func() { batch = batch[:0] }()
		if opts.DryRun {
			res.Updated += len(batch)
			return nil
		}
		updated, failures, err := store.UpdateChunkFields(ctx, indexName, batch)
		if err != nil {
			return err
		}
		res.Updated += updated
		res.Failed += len(failures)
		for _, failure := range failures {
			fmt.Fprintf(out, "retokenize: update failed: %s\n", failure)
		}
		return nil
	}

	scan := elasticsearch.ScanOptions{
		Fields:            sourceFields,
		ExcludeWithFields: compiledArtifactFields,
		Slice:             opts.Slice,
		Slices:            slices,
		Size:              opts.ScanSize,
	}
	err := store.ScanChunks(ctx, indexName, ds.ID, scan, func(id string, source map[string]interface{}) error {
		res.Scanned++
		fields, err := ChunkFields(tok, source)
		if err != nil {
			return fmt.Errorf("chunk %s: %w", id, err)
		}
		if changed := ChangedFields(source, fields); changed != nil {
			batch = append(batch, elasticsearch.ChunkFieldUpdate{ID: id, Fields: changed})
			if len(batch) >= opts.BatchSize {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if res.Scanned%progressEvery == 0 {
			fmt.Fprintf(out, "retokenize: %d chunks scanned, %d updated, %.0fs\n", res.Scanned, res.Updated, time.Since(started).Seconds())
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return res, err
	}
	verb := "updated"
	if opts.DryRun {
		verb = "would be updated"
	}
	fmt.Fprintf(out, "retokenize: done: %d chunks scanned, %d %s, %d failed in %.0fs\n", res.Scanned, res.Updated, verb, res.Failed, time.Since(started).Seconds())
	return res, nil
}

// Run converts every dataset in opts with the global document engine and
// database, which the caller has initialized along with the tokenizer pool.
func Run(ctx context.Context, opts Options, out io.Writer) error {
	store, ok := engine.Get().(*elasticsearch.Engine)
	if !ok {
		return fmt.Errorf("document engine %q is not Elasticsearch", engine.GetEngineType())
	}
	kbDAO := dao.NewKnowledgebaseDAO()
	failed := 0
	for _, id := range opts.DatasetIDs {
		kb, err := kbDAO.GetByID(ctx, dao.DB, id)
		if err != nil {
			return fmt.Errorf("dataset %s: %w", id, err)
		}
		ds := Dataset{ID: kb.ID, Name: kb.Name, TenantID: kb.TenantID}
		if kb.Language != nil {
			ds.Language = *kb.Language
		}
		res, err := RunDataset(ctx, store, ds, opts, out)
		if err != nil {
			return fmt.Errorf("dataset %s: %w", id, err)
		}
		failed += res.Failed
	}
	if failed > 0 {
		return fmt.Errorf("%d chunk updates failed", failed)
	}
	return nil
}
