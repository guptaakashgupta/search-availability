# Search & Availability Lab (OpenSearch + Go)

A local lab for grocery product search with per-store availability. All
products, brands and store IDs are made up.

## Run it

```sh
docker compose up -d          # OpenSearch :9200, Dashboards :5601 (localhost only, security off)
go run . setup                # products_v1 + alias "products" + 28 seed products
go run . search -q milk -store S001
go run . search -cat dairy/milk -store S002 -show-oos
go run . search -q "bee hoon" -debug    # prints the query JSON too
go run . suggest -q ka
go run . stock -sku SKU001 -store S001 -in=false
go run . reindex              # products_v1 -> v2, atomic alias swap
go test ./...
docker compose down -v        # tear down
```

## Files

| File | What it teaches |
|---|---|
| `index.json` | Analyzers (synonyms at search time only, edge n-gram autocomplete), `text` vs `keyword`, `dynamic: strict` |
| `products.json` | Source-system shape: one category path, list of stores |
| `main.go` `toDoc` | Flattening to a search document; ancestor category paths |
| `main.go` `buildSearch` | `must` = relevance, `filter` = hard constraints (cached, unscored), `function_score` = popularity boost, `aggs` = facets |
| `main.go` `stockScript` | Availability updates rewrite the whole doc, so debounce them in production |
| `main.go` `runReindex` | Zero-downtime mapping change via alias swap |

## Poke at it in Dashboards Dev Tools (http://localhost:5601/app/dev_tools)

```
GET products/_analyze
{ "analyzer": "product_search", "text": "bee hoon" }

GET products/_analyze
{ "analyzer": "autocomplete", "text": "Kaya" }

GET products/_explain/SKU016
{ "query": { "multi_match": { "query": "kopi", "type": "cross_fields",
  "fields": ["name^3", "brand^2", "category_names"], "operator": "and" } } }

GET products/_search
{ "size": 0, "aggs": { "per_store": { "terms": { "field": "available_store_ids" } } } }
```

## Exercises

1. `go run . search -q kopi` returns Teh Tarik because its brand is "Straits Kopi".
   Use `_explain` to confirm, then decide: lower the `brand` boost, or accept it?
2. `search -q yogurt` returns 0 hits (the product is "Yoghurt"). Add
   `"yogurt, yoghurt"` to the synonyms in `index.json`, run `go run . reindex`,
   and search again. The old index is kept for rollback.
3. `search -q "banana"` fails but `bananas` works. Try adding a `stemmer`
   filter to both analyzers, then reindex. What does `_analyze` show now?
4. Replace `available_store_ids` with a `nested` field
   `stores: [{id, qty, price}]` so each store can have its own price. What has
   to change in the query, the facets and the stock script?
