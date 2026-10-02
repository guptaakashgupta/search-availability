// Command lab is a small OpenSearch playground for grocery product search
// with per-store availability. See README.md for the walkthrough.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// alias is what the app queries; the physical index behind it (products_vN)
// can be swapped without the app noticing.
const alias = "products"

//go:embed index.json
var indexBody []byte

//go:embed products.json
var productsJSON []byte

var (
	baseURL = envOr("OPENSEARCH_URL", "http://localhost:9200")
	client  = &http.Client{Timeout: 30 * time.Second}
)

type m = map[string]any

// seedProduct is the source-system shape (think PIM + inventory).
type seedProduct struct {
	SKU           string   `json:"sku"`
	Name          string   `json:"name"`
	Brand         string   `json:"brand"`
	Category      string   `json:"category"`
	CategoryNames string   `json:"category_names"`
	Price         float64  `json:"price"`
	Popularity    int      `json:"popularity"`
	Stores        []string `json:"stores"`
}

// productDoc is the flattened, search-shaped document stored in OpenSearch.
type productDoc struct {
	SKU               string   `json:"sku"`
	Name              string   `json:"name"`
	Brand             string   `json:"brand"`
	Categories        []string `json:"categories"`
	CategoryNames     string   `json:"category_names"`
	Price             float64  `json:"price"`
	Popularity        int      `json:"popularity"`
	AvailableStoreIDs []string `json:"available_store_ids"`
	OnlineInStock     bool     `json:"online_in_stock"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "setup":
		err = setup()
	case "search":
		err = runSearch(args)
	case "suggest":
		err = runSuggest(args)
	case "stock":
		err = runStock(args)
	case "reindex":
		err = runReindex()
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: go run . <command> [flags]
  setup                                      drop products_v*, create products_v1 + alias, seed data
  search  -q milk -store S001 -cat dairy     full-text search with facets (-show-oos to demote instead of hide)
  suggest -q mi -store S001                  autocomplete on name prefixes
  stock   -sku SKU001 -store S001 -in=false  mark a SKU in/out of stock at a store
  reindex                                    copy to products_v(N+1) and swap the alias`)
	os.Exit(2)
}

func setup() error {
	if err := call(http.MethodDelete, "/products_v*", nil, nil); err != nil {
		return err
	}
	if err := send(http.MethodPut, "/products_v1", "application/json", indexBody, nil); err != nil {
		return err
	}
	aliasReq := m{"actions": []m{{"add": m{"index": "products_v1", "alias": alias}}}}
	if err := call(http.MethodPost, "/_aliases", aliasReq, nil); err != nil {
		return err
	}

	var seeds []seedProduct
	if err := json.Unmarshal(productsJSON, &seeds); err != nil {
		return fmt.Errorf("parse products.json: %w", err)
	}
	var bulk bytes.Buffer
	for _, p := range seeds {
		meta, _ := json.Marshal(m{"index": m{"_id": p.SKU}})
		doc, err := json.Marshal(toDoc(p))
		if err != nil {
			return fmt.Errorf("marshal %s: %w", p.SKU, err)
		}
		bulk.Write(meta)
		bulk.WriteByte('\n')
		bulk.Write(doc)
		bulk.WriteByte('\n')
	}
	var res struct {
		Errors bool `json:"errors"`
	}
	if err := send(http.MethodPost, "/"+alias+"/_bulk?refresh=true", "application/x-ndjson", bulk.Bytes(), &res); err != nil {
		return err
	}
	if res.Errors {
		return errors.New("bulk request had item errors; check mapping vs products.json")
	}
	fmt.Printf("indexed %d products into products_v1 (alias %q)\n", len(seeds), alias)
	return nil
}

func toDoc(p seedProduct) productDoc {
	stores := append([]string{}, p.Stores...) // never null, the stock script appends to it
	return productDoc{
		SKU:               p.SKU,
		Name:              p.Name,
		Brand:             p.Brand,
		Categories:        categoryAncestors(p.Category),
		CategoryNames:     p.CategoryNames,
		Price:             p.Price,
		Popularity:        p.Popularity,
		AvailableStoreIDs: stores,
		OnlineInStock:     len(stores) > 0,
	}
}

// categoryAncestors turns "dairy/milk/fresh-milk" into every ancestor path so a
// single term filter on "dairy/milk" matches everything beneath it.
func categoryAncestors(path string) []string {
	parts := strings.Split(path, "/")
	out := make([]string, len(parts))
	for i := range parts {
		out[i] = strings.Join(parts[:i+1], "/")
	}
	return out
}

// buildSearch is the heart of the lab: text relevance in must, hard
// constraints in filter, business boosts via function_score.
func buildSearch(q, store, category string, hideOOS bool) m {
	must := m{"match_all": m{}}
	if q != "" {
		must = m{"multi_match": m{
			"query":    q,
			"type":     "cross_fields",
			"fields":   []string{"name^3", "brand^2", "category_names"},
			"operator": "and",
		}}
	}
	filter := []any{}
	should := []any{}
	if category != "" {
		filter = append(filter, m{"term": m{"categories": category}})
	}
	if store != "" {
		atStore := m{"term": m{"available_store_ids": store}}
		if hideOOS {
			filter = append(filter, atStore)
		} else {
			should = append(should, m{"constant_score": m{"filter": atStore, "boost": 5}})
		}
	}
	return m{
		"size": 10,
		"query": m{"function_score": m{
			"query":              m{"bool": m{"must": must, "filter": filter, "should": should}},
			"field_value_factor": m{"field": "popularity", "modifier": "log1p", "missing": 0},
			"boost_mode":         "sum",
		}},
		"aggs": m{
			"brands": m{"terms": m{"field": "brand.raw", "size": 10}},
			"price": m{"range": m{"field": "price", "ranges": []m{
				{"key": "under $3", "to": 3}, {"key": "$3-6", "from": 3, "to": 6}, {"key": "$6+", "from": 6},
			}}},
		},
	}
}

type bucketAgg struct {
	Buckets []struct {
		Key      string `json:"key"`
		DocCount int    `json:"doc_count"`
	} `json:"buckets"`
}

type searchResponse struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []struct {
			Score  float64    `json:"_score"`
			Source productDoc `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
	Aggregations struct {
		Brands bucketAgg `json:"brands"`
		Price  bucketAgg `json:"price"`
	} `json:"aggregations"`
}

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	q := fs.String("q", "", "search text")
	store := fs.String("store", "", "store id, e.g. S001")
	cat := fs.String("cat", "", "category path, e.g. dairy/milk")
	showOOS := fs.Bool("show-oos", false, "demote out-of-stock items instead of hiding them")
	debug := fs.Bool("debug", false, "print the query JSON")
	fs.Parse(args)

	body := buildSearch(*q, *store, *cat, !*showOOS)
	if *debug {
		pretty, _ := json.MarshalIndent(body, "", "  ")
		fmt.Println(string(pretty))
	}
	var res searchResponse
	if err := call(http.MethodPost, "/"+alias+"/_search", body, &res); err != nil {
		return err
	}

	fmt.Printf("%d hits for %q store=%q cat=%q\n", res.Hits.Total.Value, *q, *store, *cat)
	for _, h := range res.Hits.Hits {
		d := h.Source
		note := ""
		if *store != "" && !slices.Contains(d.AvailableStoreIDs, *store) {
			note = "  [not at " + *store + "]"
		}
		fmt.Printf("  %6.2f  %s  %-32s %-16s $%5.2f%s\n", h.Score, d.SKU, d.Name, d.Brand, d.Price, note)
	}
	printFacet("brands", res.Aggregations.Brands)
	printFacet("price ", res.Aggregations.Price)
	return nil
}

func printFacet(label string, a bucketAgg) {
	parts := make([]string, 0, len(a.Buckets))
	for _, b := range a.Buckets {
		if b.DocCount > 0 {
			parts = append(parts, fmt.Sprintf("%s (%d)", b.Key, b.DocCount))
		}
	}
	fmt.Printf("  %s: %s\n", label, strings.Join(parts, ", "))
}

func runSuggest(args []string) error {
	fs := flag.NewFlagSet("suggest", flag.ExitOnError)
	q := fs.String("q", "", "prefix typed so far")
	store := fs.String("store", "", "only suggest items available at this store")
	fs.Parse(args)

	filter := []any{}
	if *store != "" {
		filter = append(filter, m{"term": m{"available_store_ids": *store}})
	}
	body := m{
		"size":    8,
		"_source": []string{"name"},
		"query": m{"bool": m{
			"must":   m{"match": m{"name.auto": m{"query": *q, "operator": "and"}}},
			"filter": filter,
		}},
	}
	var res searchResponse
	if err := call(http.MethodPost, "/"+alias+"/_search", body, &res); err != nil {
		return err
	}
	for _, h := range res.Hits.Hits {
		fmt.Println("  " + h.Source.Name)
	}
	return nil
}

// stockScript is a partial update, but Lucene segments are immutable, so
// OpenSearch still rewrites the whole document. This is why stock updates
// should be debounced and only sent when availability actually flips.
const stockScript = `
if (params.in) {
  if (!ctx._source.available_store_ids.contains(params.store)) { ctx._source.available_store_ids.add(params.store); }
} else {
  ctx._source.available_store_ids.removeIf(s -> s == params.store);
}
ctx._source.online_in_stock = !ctx._source.available_store_ids.isEmpty();`

func runStock(args []string) error {
	fs := flag.NewFlagSet("stock", flag.ExitOnError)
	sku := fs.String("sku", "", "SKU id")
	store := fs.String("store", "", "store id")
	in := fs.Bool("in", true, "true = in stock, false = out of stock")
	fs.Parse(args)
	if *sku == "" || *store == "" {
		return errors.New("stock needs -sku and -store")
	}
	body := m{"script": m{"source": stockScript, "params": m{"store": *store, "in": *in}}}
	var res struct {
		Result string `json:"result"`
	}
	if err := call(http.MethodPost, "/"+alias+"/_update/"+*sku+"?refresh=true", body, &res); err != nil {
		return err
	}
	fmt.Printf("%s at %s in_stock=%t: %s\n", *sku, *store, *in, res.Result)
	return nil
}

func runReindex() error {
	var current map[string]any
	if err := call(http.MethodGet, "/_alias/"+alias, nil, &current); err != nil {
		return err
	}
	if len(current) != 1 {
		return fmt.Errorf("expected alias %q on exactly one index, got %d", alias, len(current))
	}
	var oldIdx string
	for k := range current {
		oldIdx = k
	}
	n, err := strconv.Atoi(strings.TrimPrefix(oldIdx, "products_v"))
	if err != nil {
		return fmt.Errorf("unexpected index name %q", oldIdx)
	}
	newIdx := fmt.Sprintf("products_v%d", n+1)

	// In real life index.json would differ here (new analyzer, new field).
	if err := send(http.MethodPut, "/"+newIdx, "application/json", indexBody, nil); err != nil {
		return err
	}
	var re struct {
		Total int `json:"total"`
	}
	copyReq := m{"source": m{"index": oldIdx}, "dest": m{"index": newIdx}}
	if err := call(http.MethodPost, "/_reindex?refresh=true", copyReq, &re); err != nil {
		return err
	}
	// Remove + add in one request is atomic: readers never see zero or two indices.
	swap := m{"actions": []m{
		{"remove": m{"index": oldIdx, "alias": alias}},
		{"add": m{"index": newIdx, "alias": alias}},
	}}
	if err := call(http.MethodPost, "/_aliases", swap, nil); err != nil {
		return err
	}
	fmt.Printf("copied %d docs %s -> %s, alias %q now points to %s (old index kept for rollback)\n",
		re.Total, oldIdx, newIdx, alias, newIdx)
	return nil
}

// call JSON-encodes body (if any) and decodes the response into out (if non-nil).
func call(method, path string, body, out any) error {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
	}
	return send(method, path, "application/json", b, out)
}

func send(method, path, contentType string, body []byte, out any) error {
	req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w (is `docker compose up -d` running?)", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
