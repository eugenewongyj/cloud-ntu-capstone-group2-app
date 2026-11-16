// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of this
// software and associated documentation files (the "Software"), to deal in the Software
// without restriction, including without limitation the rights to use, copy, modify,
// merge, publish, distribute, sublicense, and/or sell copies of the Software, and to
// permit persons to whom the Software is furnished to do so.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED,
// INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A
// PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT
// HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
// OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
// SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package repository

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws-containers/retail-store-sample-app/catalog/config"
	"github.com/aws-containers/retail-store-sample-app/catalog/model"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/opensearch-project/opensearch-go/v2"
	"github.com/opensearch-project/opensearch-go/v2/opensearchapi"
	"github.com/opensearch-project/opensearch-go/v2/signer/awsv2"
)

// SearchRepository interface for search operations
type SearchRepository interface {
	SearchProducts(keyword string, page, size int, ctx context.Context) ([]model.Product, error)
	Reindex() error
}

// OpenSearchRepository implements SearchRepository
type OpenSearchRepository struct {
	client    *opensearch.Client
	indexName string
	// serverless is true for Amazon OpenSearch Serverless, which does not
	// support some APIs/settings (cluster info, _cat, shard settings, refresh).
	serverless bool
}

// ProductDocument represents the product structure stored in OpenSearch
type ProductDocument struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Price       int      `json:"price"`
	Tags        []string `json:"tags"`
}

// SearchResponse represents the OpenSearch search response structure
type SearchResponse struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []struct {
			Source ProductDocument `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

// NewOpenSearchRepository creates a new OpenSearch repository
func NewOpenSearchRepository(config config.OpenSearchConfiguration) (*OpenSearchRepository, error) {
	cfg := opensearch.Config{
		Addresses: []string{config.Endpoint},
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: config.TLSSkipVerify},
		},
	}

	serverless := false

	if config.Auth == "sigv4" {
		// Sign every request with the pod's short-lived AWS credentials (Pod Identity)
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}
		if awsCfg.Region == "" {
			return nil, fmt.Errorf("AWS region is not set (set AWS_REGION)")
		}
		signer, err := awsv2.NewSignerWithService(awsCfg, config.AWSService)
		if err != nil {
			return nil, fmt.Errorf("failed to create AWS request signer: %w", err)
		}
		cfg.Signer = signer
		serverless = config.AWSService == "aoss"

		fmt.Printf("Connecting to OpenSearch with AWS SigV4 (service %s)\n", config.AWSService)
	} else if config.Username != "" && config.Password != "" {
		// Add authentication if provided
		cfg.Username = config.Username
		cfg.Password = config.Password

		fmt.Printf("Connecting to OpenSearch as %s\n", config.Username)
	}

	client, err := opensearch.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create OpenSearch client: %w", err)
	}

	// Test connection. OpenSearch Serverless has no cluster info API, so check
	// the index instead (200 = exists, 404 = not yet created; both mean we're connected).
	var res *opensearchapi.Response
	if serverless {
		res, err = client.Indices.Exists([]string{config.IndexName})
		if err == nil && res.StatusCode == http.StatusNotFound {
			res.Body.Close()
			res, err = nil, nil
		}
	} else {
		res, err = client.Info()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to OpenSearch: %w", err)
	}
	if res != nil {
		defer res.Body.Close()

		if res.IsError() {
			return nil, fmt.Errorf("OpenSearch connection error: %s", res.String())
		}
	}

	fmt.Println("Successfully connected to OpenSearch")

	return &OpenSearchRepository{
		client:     client,
		indexName:  config.IndexName,
		serverless: serverless,
	}, nil
}

// InitializeData creates the index and loads product data into OpenSearch
// If the index already exists and contains documents, indexing is skipped.
func (r *OpenSearchRepository) InitializeData() error {
	ctx := context.Background()

	// Check if index exists
	existsRes, err := r.client.Indices.Exists([]string{r.indexName})
	if err != nil {
		return fmt.Errorf("failed to check index existence: %w", err)
	}
	defer existsRes.Body.Close()

	// If index exists, check if it has documents
	if !existsRes.IsError() {
		// Uses the _count API (works on both OpenSearch and OpenSearch Serverless;
		// _cat/count is not available on Serverless)
		countReq := opensearchapi.CountRequest{
			Index: []string{r.indexName},
		}
		countRes, err := countReq.Do(ctx, r.client)
		if err != nil {
			return fmt.Errorf("failed to check index document count: %w", err)
		}
		defer countRes.Body.Close()

		if countRes.IsError() {
			// Don't delete an index we couldn't count
			return fmt.Errorf("failed to check index document count: %s", countRes.String())
		}

		var countResponse struct {
			Count int `json:"count"`
		}
		if err := json.NewDecoder(countRes.Body).Decode(&countResponse); err != nil {
			return fmt.Errorf("failed to parse index document count: %w", err)
		}
		if countResponse.Count > 0 {
			fmt.Printf("OpenSearch index '%s' already exists with %d documents, skipping re-index\n", r.indexName, countResponse.Count)
			return nil
		}

		// Index exists but is empty, delete and recreate
		deleteRes, err := r.client.Indices.Delete([]string{r.indexName})
		if err != nil {
			return fmt.Errorf("failed to delete existing index: %w", err)
		}
		defer deleteRes.Body.Close()
		fmt.Println("Deleted empty OpenSearch index, will recreate")
	}

	return r.createAndPopulateIndex(ctx)
}

// createAndPopulateIndex creates the index with mappings and loads product data.
func (r *OpenSearchRepository) createAndPopulateIndex(ctx context.Context) error {
	// Create index with mappings
	// OpenSearch Serverless manages shards and replicas itself
	shardSettings := `
			"number_of_shards": 1,
			"number_of_replicas": 0,`
	if r.serverless {
		shardSettings = ""
	}

	mapping := `{
		"settings": {` + shardSettings + `
			"analysis": {
				"analyzer": {
					"product_analyzer": {
						"type": "custom",
						"tokenizer": "standard",
						"filter": ["lowercase", "stop", "snowball"]
					}
				}
			}
		},
		"mappings": {
			"properties": {
				"id": { "type": "keyword" },
				"name": { 
					"type": "text",
					"analyzer": "product_analyzer",
					"fields": {
						"keyword": { "type": "keyword" }
					}
				},
				"description": { 
					"type": "text",
					"analyzer": "product_analyzer"
				},
				"price": { "type": "integer" },
				"tags": { "type": "keyword" }
			}
		}
	}`

	createRes, err := r.client.Indices.Create(
		r.indexName,
		r.client.Indices.Create.WithBody(strings.NewReader(mapping)),
		r.client.Indices.Create.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
	}
	defer createRes.Body.Close()

	if createRes.IsError() {
		return fmt.Errorf("failed to create index: %s", createRes.String())
	}

	fmt.Println("Created OpenSearch index with mappings")

	// Load products from JSON file
	products, err := LoadProductData()
	if err != nil {
		return fmt.Errorf("failed to load product data: %w", err)
	}

	// Bulk index products
	var bulkBody strings.Builder
	for _, product := range products {
		// Action line
		action := fmt.Sprintf(`{"index":{"_index":"%s","_id":"%s"}}`, r.indexName, product.ID)
		bulkBody.WriteString(action)
		bulkBody.WriteString("\n")

		// Document line
		doc := ProductDocument{
			ID:          product.ID,
			Name:        product.Name,
			Description: product.Description,
			Price:       product.Price,
			Tags:        product.Tags,
		}
		docJSON, err := json.Marshal(doc)
		if err != nil {
			return fmt.Errorf("failed to marshal product: %w", err)
		}
		bulkBody.WriteString(string(docJSON))
		bulkBody.WriteString("\n")
	}

	bulkReq := opensearchapi.BulkRequest{
		Body: strings.NewReader(bulkBody.String()),
	}
	if !r.serverless {
		// OpenSearch Serverless does not support the refresh parameter
		bulkReq.Refresh = "true"
	}

	bulkRes, err := bulkReq.Do(ctx, r.client)
	if err != nil {
		return fmt.Errorf("failed to bulk index products: %w", err)
	}
	defer bulkRes.Body.Close()

	if bulkRes.IsError() {
		return fmt.Errorf("bulk indexing error: %s", bulkRes.String())
	}

	fmt.Printf("Successfully indexed %d products into OpenSearch\n", len(products))
	return nil
}

// Reindex drops the existing index and recreates it with fresh data.
func (r *OpenSearchRepository) Reindex() error {
	ctx := context.Background()

	// Delete existing index if it exists
	existsRes, err := r.client.Indices.Exists([]string{r.indexName})
	if err != nil {
		return fmt.Errorf("failed to check index existence: %w", err)
	}
	defer existsRes.Body.Close()

	if !existsRes.IsError() {
		deleteRes, err := r.client.Indices.Delete([]string{r.indexName})
		if err != nil {
			return fmt.Errorf("failed to delete existing index: %w", err)
		}
		defer deleteRes.Body.Close()
		fmt.Println("Deleted existing OpenSearch index for reindex")
	}

	return r.createAndPopulateIndex(ctx)
}

// SearchProducts searches for products matching the keyword with pagination
func (r *OpenSearchRepository) SearchProducts(keyword string, page, size int, ctx context.Context) ([]model.Product, error) {
	// Calculate offset for pagination
	from := (page - 1) * size

	// Build the search query
	query := map[string]interface{}{
		"query": map[string]interface{}{
			"multi_match": map[string]interface{}{
				"query":     keyword,
				"fields":    []string{"name^2", "description", "tags"},
				"fuzziness": "AUTO",
			},
		},
		"from": from,
		"size": size,
	}

	queryJSON, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal search query: %w", err)
	}

	// Execute search
	searchReq := opensearchapi.SearchRequest{
		Index: []string{r.indexName},
		Body:  bytes.NewReader(queryJSON),
	}

	res, err := searchReq.Do(ctx, r.client)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		return nil, fmt.Errorf("search error: %s", res.String())
	}

	// Parse response
	var searchResponse SearchResponse
	if err := json.NewDecoder(res.Body).Decode(&searchResponse); err != nil {
		return nil, fmt.Errorf("failed to parse search response: %w", err)
	}

	// Convert to Product model
	products := make([]model.Product, 0, len(searchResponse.Hits.Hits))
	for _, hit := range searchResponse.Hits.Hits {
		tags := make([]model.Tag, len(hit.Source.Tags))
		for i, tagName := range hit.Source.Tags {
			tags[i] = model.Tag{Name: tagName}
		}

		products = append(products, model.Product{
			ID:          hit.Source.ID,
			Name:        hit.Source.Name,
			Description: hit.Source.Description,
			Price:       hit.Source.Price,
			Tags:        tags,
		})
	}

	return products, nil
}
