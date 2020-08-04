package search

import (
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"path"
	"time"

	log "github.com/go-pkgz/lgr"

	"github.com/blevesearch/bleve"
	bleveCustom "github.com/blevesearch/bleve/analysis/analyzer/custom"
	bleveStandard "github.com/blevesearch/bleve/analysis/analyzer/standard"
	bleveEn "github.com/blevesearch/bleve/analysis/lang/en"
	bleveRu "github.com/blevesearch/bleve/analysis/lang/ru"
	bleveSingle "github.com/blevesearch/bleve/analysis/tokenizer/single"

	"github.com/blevesearch/bleve/analysis/token/lowercase"
	"github.com/blevesearch/bleve/mapping"
	"github.com/pkg/errors"
)

const urlFieldName = "url"
const textFieldName = "text"

// Available text analyzers.
// Bleve supports a bit more languages that may be added,
// see https://github.com/blevesearch/bleve/tree/master/analysis/lang
var analyzerMapping = map[string]string{
	"standard": bleveStandard.Name,
	"english":  bleveEn.AnalyzerName,
	"russian":  bleveRu.AnalyzerName,
}

type bleveBatch struct {
	*bleve.Batch
}

type bleveIndexer struct {
	bleve.Index
}

func (b bleveBatch) Index(id string, data *DocumentComment) error {
	return b.Batch.Index(id, data)
}

func newBleve(indexPath, analyzer string) (s *bufferedEngine, err error) {
	if _, ok := analyzerMapping[analyzer]; !ok {
		analyzers := make([]string, 0, len(analyzerMapping))
		for k := range analyzerMapping {
			analyzers = append(analyzers, k)
		}
		return nil, errors.Errorf("Unknown analyzer: %q. Available analyzers for bleve: %v", analyzer, analyzers)
	}
	var index bleve.Index

	if st, errOpen := os.Stat(indexPath); os.IsNotExist(errOpen) {
		log.Printf("[INFO] creating new search index %s", indexPath)
		index, err = bleve.New(indexPath, createIndexMapping(analyzerMapping[analyzer]))
	} else if errOpen == nil {
		if !st.IsDir() {
			return nil, errors.Errorf("index path shoule be a directory")
		}
		log.Printf("[INFO] opening existing search index %s", indexPath)
		index, err = bleve.Open(indexPath)
	} else {
		err = errOpen
	}

	if err != nil {
		return nil, errors.Wrap(err, "cannot create/open index")
	}

	eng := &bufferedEngine{
		index:         bleveIndexer{index},
		queueNotifier: make(chan bool),
		flushEvery:    2 * time.Second,
		flushCount:    100,
		indexPath:     indexPath,
	}

	go eng.indexDocumentWorker()

	return eng, nil
}

func newBleveService(params SearcherParams) (s Service, err error) {
	encodeSiteID := func(siteID string) string {
		h := fnv.New32().Sum([]byte(siteID))
		return hex.EncodeToString(h)
	}

	shards := map[string]*bufferedEngine{}

	for _, siteID := range params.Sites {
		fpath := path.Join(params.IndexPath, encodeSiteID(siteID))
		shards[siteID], err = newBleve(fpath, params.Analyzer)
		if err != nil {
			return nil, err
		}
	}
	return newMultiplexer(shards, params.Type), err
}

func (idx bleveIndexer) NewBatch() indexerBatch {
	return bleveBatch{idx.Index.NewBatch()}
}

func (idx bleveIndexer) Batch(batch indexerBatch) error {
	b := batch.(bleveBatch).Batch
	return idx.Index.Batch(b)
}

func convertBleveSerp(bleveResult *bleve.SearchResult) *ResultPage {
	result := ResultPage{
		Total:     bleveResult.Total,
		Documents: make([]ResultDoc, 0, len(bleveResult.Hits)),
	}
	for _, r := range bleveResult.Hits {
		url, hasURL := r.Fields[urlFieldName].(string)
		if !hasURL {
			panic(fmt.Sprintf("cannot find %q in %v", urlFieldName, r.Fields))
		}

		d := ResultDoc{
			ID:      r.ID,
			Matches: []TokenMatch{},
			PostURL: url,
		}

		if highlight, has := r.Locations[textFieldName]; has {
			for _, locs := range highlight {
				for _, loc := range locs {
					d.Matches = append(d.Matches, TokenMatch{
						Start: loc.Start,
						End:   loc.End,
					})
				}
			}
		}

		result.Documents = append(result.Documents, d)
	}
	return &result
}

func textMapping(analyzer string, doStore bool) *mapping.FieldMapping {
	textFieldMapping := bleve.NewTextFieldMapping()
	textFieldMapping.Store = doStore
	textFieldMapping.Analyzer = analyzer
	textFieldMapping.IncludeTermVectors = true
	return textFieldMapping
}

func commentDocumentMapping(textAnalyzer string) *mapping.DocumentMapping {
	commentMapping := bleve.NewDocumentMapping()

	commentMapping.AddFieldMappingsAt(textFieldName, textMapping(textAnalyzer, false))
	commentMapping.AddFieldMappingsAt("username", textMapping("keyword_lower", true))
	commentMapping.AddFieldMappingsAt(urlFieldName, textMapping("keyword_lower", true))
	return commentMapping
}

func (idx bleveIndexer) Search(req *Request) (*ResultPage, error) {
	bQuery := bleve.NewQueryStringQuery(req.Query)
	bReq := bleve.NewSearchRequestOptions(bQuery, req.Limit, req.From, false)

	if validateSortField(req.SortBy, "timestamp") {
		bReq.SortBy([]string{req.SortBy})
	} else if req.SortBy != "" {
		log.Printf("[WARN] unknown sort field %q", req.SortBy)
	}

	bReq.Fields = append(bReq.Fields, urlFieldName)
	bReq.Highlight = bleve.NewHighlight()
	bReq.Highlight.AddField(textFieldName)

	serp, err := idx.Index.Search(bReq)
	if err != nil {
		return nil, errors.Wrap(err, "bleve search error")
	}
	log.Printf("[INFO] found %d documents for query %q in %s",
		serp.Total, req.Query, serp.Took.String())

	result := convertBleveSerp(serp)
	return result, nil
}

func (idx bleveIndexer) Delete(id string) error {
	return idx.Index.Delete(id)
}

func (idx bleveIndexer) Close() error {
	return idx.Index.Close()
}

func createIndexMapping(textAnalyzer string) mapping.IndexMapping {
	indexMapping := bleve.NewIndexMapping()
	err := indexMapping.AddCustomAnalyzer("keyword_lower", map[string]interface{}{
		"type":      bleveCustom.Name,
		"tokenizer": bleveSingle.Name,
		"token_filters": []string{
			lowercase.Name,
		},
	})
	if err != nil {
		panic(fmt.Sprintf("error adding bleve analyzer %v", err))
	}
	indexMapping.AddDocumentMapping(commentDocType, commentDocumentMapping(textAnalyzer))

	return indexMapping
}