package lsifstore

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/keegancsmith/sqlf"
	"github.com/opentracing/opentracing-go/log"

	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/stores/dbstore"
	"github.com/sourcegraph/sourcegraph/internal/database/batch"
	"github.com/sourcegraph/sourcegraph/internal/observation"
	"github.com/sourcegraph/sourcegraph/internal/types"
	"github.com/sourcegraph/sourcegraph/lib/codeintel/precise"
)

// WriteDocumentationPages is called (transactionally) from the precise-code-intel-worker.
func (s *Store) WriteDocumentationPages(ctx context.Context, upload dbstore.Upload, repo *types.Repo, documentationPages chan *precise.DocumentationPageData) (err error) {
	ctx, traceLog, endObservation := s.operations.writeDocumentationPages.WithAndLogger(ctx, &err, observation.Args{LogFields: []log.Field{
		log.Int("bundleID", upload.ID),
	}})
	defer endObservation(1, observation.Args{})

	tx, err := s.Transact(ctx)
	if err != nil {
		return err
	}
	defer func() { err = tx.Done(err) }()

	// Create temporary table symmetric to lsif_data_documentation_pages without the dump id
	if err := tx.Exec(ctx, sqlf.Sprintf(writeDocumentationPagesTemporaryTableQuery)); err != nil {
		return err
	}

	var count uint32
	inserter := func(inserter *batch.Inserter) error {
		for page := range documentationPages {
			// Write to search table.
			if err := tx.WriteDocumentationSearch(ctx, NewDocumentationSearchInfo(upload), repo, page); err != nil {
				return errors.Wrap(err, "WriteDocumentationSearch")
			}

			data, err := s.serializer.MarshalDocumentationPageData(page)
			if err != nil {
				return err
			}

			if err := inserter.Insert(ctx, page.Tree.PathID, data); err != nil {
				return err
			}

			atomic.AddUint32(&count, 1)
		}
		return nil
	}

	// Bulk insert all the unique column values into the temporary table
	if err := withBatchInserter(
		ctx,
		tx.Handle().DB(),
		"t_lsif_data_documentation_pages",
		[]string{"path_id", "data"},
		inserter,
	); err != nil {
		return err
	}
	traceLog(log.Int("numResultChunkRecords", int(count)))

	// Insert the values from the temporary table into the target table. We select a
	// parameterized dump id here since it is the same for all rows in this operation.
	return tx.Exec(ctx, sqlf.Sprintf(writeDocumentationPagesInsertQuery, upload.ID))
}

const writeDocumentationPagesTemporaryTableQuery = `
-- source: enterprise/internal/codeintel/stores/lsifstore/data_write_documentation.go:WriteDocumentationPages
CREATE TEMPORARY TABLE t_lsif_data_documentation_pages (
	path_id TEXT NOT NULL,
	data bytea NOT NULL
) ON COMMIT DROP
`

const writeDocumentationPagesInsertQuery = `
-- source: enterprise/internal/codeintel/stores/lsifstore/data_write_documentation.go:WriteDocumentationPages
INSERT INTO lsif_data_documentation_pages (dump_id, path_id, data)
SELECT %s, source.path_id, source.data
FROM t_lsif_data_documentation_pages source
`

// WriteDocumentationPathInfo is called (transactionally) from the precise-code-intel-worker.
func (s *Store) WriteDocumentationPathInfo(ctx context.Context, bundleID int, documentationPathInfo chan *precise.DocumentationPathInfoData) (err error) {
	ctx, traceLog, endObservation := s.operations.writeDocumentationPathInfo.WithAndLogger(ctx, &err, observation.Args{LogFields: []log.Field{
		log.Int("bundleID", bundleID),
	}})
	defer endObservation(1, observation.Args{})

	tx, err := s.Transact(ctx)
	if err != nil {
		return err
	}
	defer func() { err = tx.Done(err) }()

	// Create temporary table symmetric to lsif_data_documentation_path_info without the dump id
	if err := tx.Exec(ctx, sqlf.Sprintf(writeDocumentationPathInfoTemporaryTableQuery)); err != nil {
		return err
	}

	var count uint32
	inserter := func(inserter *batch.Inserter) error {
		for v := range documentationPathInfo {
			data, err := s.serializer.MarshalDocumentationPathInfoData(v)
			if err != nil {
				return err
			}

			if err := inserter.Insert(ctx, v.PathID, data); err != nil {
				return err
			}

			atomic.AddUint32(&count, 1)
		}
		return nil
	}

	// Bulk insert all the unique column values into the temporary table
	if err := withBatchInserter(
		ctx,
		tx.Handle().DB(),
		"t_lsif_data_documentation_path_info",
		[]string{"path_id", "data"},
		inserter,
	); err != nil {
		return err
	}
	traceLog(log.Int("numResultChunkRecords", int(count)))

	// Insert the values from the temporary table into the target table. We select a
	// parameterized dump id here since it is the same for all rows in this operation.
	return tx.Exec(ctx, sqlf.Sprintf(writeDocumentationPathInfoInsertQuery, bundleID))
}

const writeDocumentationPathInfoTemporaryTableQuery = `
-- source: enterprise/internal/codeintel/stores/lsifstore/data_write_documentation.go:WriteDocumentationPathInfo
CREATE TEMPORARY TABLE t_lsif_data_documentation_path_info (
	path_id TEXT NOT NULL,
	data bytea NOT NULL
) ON COMMIT DROP
`

const writeDocumentationPathInfoInsertQuery = `
-- source: enterprise/internal/codeintel/stores/lsifstore/data_write_documentation.go:WriteDocumentationPathInfo
INSERT INTO lsif_data_documentation_path_info (dump_id, path_id, data)
SELECT %s, source.path_id, source.data
FROM t_lsif_data_documentation_path_info source
`

// WriteDocumentationMappings is called (transactionally) from the precise-code-intel-worker.
func (s *Store) WriteDocumentationMappings(ctx context.Context, bundleID int, mappings chan precise.DocumentationMapping) (err error) {
	ctx, traceLog, endObservation := s.operations.writeDocumentationMappings.WithAndLogger(ctx, &err, observation.Args{LogFields: []log.Field{
		log.Int("bundleID", bundleID),
	}})
	defer endObservation(1, observation.Args{})

	tx, err := s.Transact(ctx)
	if err != nil {
		return err
	}
	defer func() { err = tx.Done(err) }()

	// Create temporary table symmetric to lsif_data_documentation_mappings without the dump id
	if err := tx.Exec(ctx, sqlf.Sprintf(writeDocumentationMappingsTemporaryTableQuery)); err != nil {
		return err
	}

	var count uint32
	inserter := func(inserter *batch.Inserter) error {
		for mapping := range mappings {
			if err := inserter.Insert(ctx, mapping.PathID, mapping.ResultID, mapping.FilePath); err != nil {
				return err
			}
			atomic.AddUint32(&count, 1)
		}
		return nil
	}

	// Bulk insert all the unique column values into the temporary table
	if err := withBatchInserter(
		ctx,
		tx.Handle().DB(),
		"t_lsif_data_documentation_mappings",
		[]string{"path_id", "result_id", "file_path"},
		inserter,
	); err != nil {
		return err
	}
	traceLog(log.Int("numRecords", int(count)))

	// Insert the values from the temporary table into the target table. We select a
	// parameterized dump id here since it is the same for all rows in this operation.
	return tx.Exec(ctx, sqlf.Sprintf(writeDocumentationMappingsInsertQuery, bundleID))
}

const writeDocumentationMappingsTemporaryTableQuery = `
-- source: enterprise/internal/codeintel/stores/lsifstore/data_write_documentation.go:WriteDocumentationMappings
CREATE TEMPORARY TABLE t_lsif_data_documentation_mappings (
	path_id TEXT NOT NULL,
	result_id integer NOT NULL,
	file_path text
) ON COMMIT DROP
`

const writeDocumentationMappingsInsertQuery = `
-- source: enterprise/internal/codeintel/stores/lsifstore/data_write_documentation.go:WriteDocumentationMappings
INSERT INTO lsif_data_documentation_mappings (dump_id, path_id, result_id, file_path)
SELECT %s, source.path_id, source.result_id, source.file_path
FROM t_lsif_data_documentation_mappings source
`

type DocumentationSearchInfo struct {
	ID             int
	RepositoryName string
	RepositoryID   int
	Indexer        string
}

func NewDocumentationSearchInfo(dumpOrUpload interface{}) DocumentationSearchInfo {
	switch v := dumpOrUpload.(type) {
	case *dbstore.Dump:
		return DocumentationSearchInfo{
			ID:             v.ID,
			RepositoryName: v.RepositoryName,
			RepositoryID:   v.RepositoryID,
			Indexer:        v.Indexer,
		}
	case *dbstore.Upload:
		return DocumentationSearchInfo{
			ID:             v.ID,
			RepositoryName: v.RepositoryName,
			RepositoryID:   v.RepositoryID,
			Indexer:        v.Indexer,
		}
	default:
		panic("invariant")
	}
}

// WriteDocumentationSearch is called (within a transaction) to write the search index for a given documentation page.
func (s *Store) WriteDocumentationSearch(ctx context.Context, meta DocumentationSearchInfo, repo *types.Repo, page *precise.DocumentationPageData) (err error) {
	ctx, traceLog, endObservation := s.operations.writeDocumentationSearch.WithAndLogger(ctx, &err, observation.Args{LogFields: []log.Field{
		log.Int("bundleID", meta.ID),
	}})
	defer endObservation(1, observation.Args{})

	// This will not always produce a proper language name, e.g. if an indexer is not named after
	// the language or is not in "lsif-$LANGUAGE" format. That's OK: in that case, the "language tag"
	// is the indexer name which is likely good enough!
	languageTag := strings.ToLower(strings.TrimPrefix(meta.Indexer, "lsif-"))

	var count uint32
	var index func(node *precise.DocumentationNode) error
	index = func(node *precise.DocumentationNode) error {
		if node.Documentation.SearchKey != "" {
			tags := []string{languageTag}
			for _, tag := range node.Documentation.Tags {
				tags = append(tags, string(tag))
			}
			err := s.Exec(ctx, sqlf.Sprintf(
				writeDocumentationSearchInsertQuery,
				meta.ID,
				node.PathID,
				meta.RepositoryName,
				node.Documentation.SearchKey,
				node.Label.String(),
				node.Detail.String(),
				strings.Join(tags, " "),
				meta.RepositoryID,
				repo.Private,
			))
			if err != nil {
				return err
			}
			count++
		}

		// Index descendants.
		for _, child := range node.Children {
			if child.Node != nil {
				if err := index(child.Node); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := index(page.Tree); err != nil {
		return err
	}
	traceLog(log.Int("numRecords", int(count)))
	return nil
}

const writeDocumentationSearchInsertQuery = `
-- source: enterprise/internal/codeintel/stores/lsifstore/data_write_documentation.go:WriteDocumentationSearch
INSERT INTO lsif_data_documentation_search (dump_id, path_id, repo_name, search_key, label, detail, tags, repo_id, repo_private)
VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)
`
