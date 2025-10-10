// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
package main

//go:generate go install google.golang.org/protobuf/cmd/protoc-gen-go
//go:generate go install google.golang.org/grpc/cmd/protoc-gen-go-grpc
//go:generate protoc --go_out=./ --go-grpc_out=./ --proto_path=../../pb ../../pb/demo.proto

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"go.opentelemetry.io/contrib/bridges/otellogrus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	otellog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	otelhooks "github.com/open-feature/go-sdk-contrib/hooks/open-telemetry/pkg"
	flagd "github.com/open-feature/go-sdk-contrib/providers/flagd/pkg"
	"github.com/open-feature/go-sdk/openfeature"
	pb "github.com/opentelemetry/opentelemetry-demo/src/product-catalog/genproto/oteldemo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

var (
	log         *logrus.Logger
	catalog     []*pb.Product
	resource    *sdkresource.Resource
	initResOnce sync.Once
	meter       metric.Meter
	// Performance metrics
	listProductsHistogram   metric.Float64Histogram
	getProductHistogram     metric.Float64Histogram
	searchProductsHistogram metric.Float64Histogram
	// Error metrics
	errorCounter          metric.Int64Counter
	unhandledErrorCounter metric.Int64Counter
	// Request counters
	productsCounter      metric.Int64Counter
	productCounter       metric.Int64Counter
	searchCounter        metric.Int64Counter
	searchResultsCounter metric.Int64Counter
)

const DEFAULT_RELOAD_INTERVAL = 10

func init() {
	log = logrus.New()
	log.Level = logrus.DebugLevel
	log.Formatter = &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "timestamp",
			logrus.FieldKeyLevel: "severity",
			logrus.FieldKeyMsg:   "message",
		},
		TimestampFormat: time.RFC3339Nano,
	}

	// Initialize OpenTelemetry log pipeline
	ctx := context.Background()
	exporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		log.Fatalf("new otlp log grpc exporter failed: %v", err)
	}

	lp := otellog.NewLoggerProvider(
		otellog.WithProcessor(otellog.NewBatchProcessor(exporter)),
		otellog.WithResource(initResource()),
	)

	// Create an otellogrus.Hook and use it in your application
	hook := otellogrus.NewHook("checkout", otellogrus.WithLoggerProvider(lp))

	// Set the newly created hook as a global logrus hook
	log.AddHook(hook)

	loadProductCatalog()
	// Make sure everything is flushed at exit
	go func() {
		<-context.Background().Done()
		_ = lp.Shutdown(context.Background())
	}()
}

func initResource() *sdkresource.Resource {
	initResOnce.Do(func() {
		extraResources, _ := sdkresource.New(
			context.Background(),
			sdkresource.WithOS(),
			sdkresource.WithProcess(),
			sdkresource.WithContainer(),
			sdkresource.WithHost(),
		)
		resource, _ = sdkresource.Merge(
			sdkresource.Default(),
			extraResources,
		)
	})
	return resource
}

func initTracerProvider() *sdktrace.TracerProvider {
	ctx := context.Background()

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		log.Fatal("new otlp trace grpc exporter failed", "error", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(initResource()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp
}

func initMeterProvider() *sdkmetric.MeterProvider {
	ctx := context.Background()

	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		log.Fatal("new otlp metric grpc exporter failed", "error", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		sdkmetric.WithResource(initResource()),
	)
	otel.SetMeterProvider(mp)

	// Initialize meter for custom metrics
	meter = mp.Meter("product-catalog")

	// Initialize histograms for performance metrics
	listProductsHistogram, _ = meter.Float64Histogram(
		"product_catalog.list_products.duration",
		metric.WithDescription("Duration of ListProducts operation in milliseconds"),
		metric.WithUnit("ms"),
	)
	getProductHistogram, _ = meter.Float64Histogram(
		"product_catalog.get_product.duration",
		metric.WithDescription("Duration of GetProduct operation in milliseconds"),
		metric.WithUnit("ms"),
	)
	searchProductsHistogram, _ = meter.Float64Histogram(
		"product_catalog.search_products.duration",
		metric.WithDescription("Duration of SearchProducts operation in milliseconds"),
		metric.WithUnit("ms"),
	)

	// Initialize error counters
	errorCounter, _ = meter.Int64Counter(
		"product_catalog.errors.total",
		metric.WithDescription("Total number of handled errors"),
	)
	unhandledErrorCounter, _ = meter.Int64Counter(
		"product_catalog.errors.unhandled",
		metric.WithDescription("Total number of unhandled errors"),
	)

	// Initialize request counters
	productsCounter, _ = meter.Int64Counter(
		"product_catalog.list_products.count",
		metric.WithDescription("Total number of ListProducts calls"),
	)
	productCounter, _ = meter.Int64Counter(
		"product_catalog.get_product.count",
		metric.WithDescription("Total number of GetProduct calls"),
	)
	searchCounter, _ = meter.Int64Counter(
		"product_catalog.search_products.count",
		metric.WithDescription("Total number of SearchProducts calls"),
	)
	searchResultsCounter, _ = meter.Int64Counter(
		"product_catalog.search_products.results",
		metric.WithDescription("Total number of search results returned"),
	)

	return mp
}

func main() {
	tp := initTracerProvider()
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			log.Fatalf("Tracer Provider Shutdown: %v", err)
		}
		log.Println("Shutdown tracer provider")
	}()

	mp := initMeterProvider()
	defer func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			log.Fatalf("Error shutting down meter provider: %v", err)
		}
		log.Println("Shutdown meter provider")
	}()

	openfeature.AddHooks(otelhooks.NewTracesHook())
	err := openfeature.SetProvider(flagd.NewProvider())
	if err != nil {
		log.Fatal(err)
	}

	err = runtime.Start(runtime.WithMinimumReadMemStatsInterval(time.Second))
	if err != nil {
		log.Fatal(err)
	}

	svc := &productCatalog{}
	var port string
	mustMapEnv(&port, "PRODUCT_CATALOG_PORT")

	log.Infof("Product Catalog gRPC server started on port: %s", port)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		log.Fatalf("TCP Listen: %v", err)
	}

	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	)

	reflection.Register(srv)

	pb.RegisterProductCatalogServiceServer(srv, svc)
	healthpb.RegisterHealthServer(srv, svc)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGKILL)
	defer cancel()

	go func() {
		if err := srv.Serve(ln); err != nil {
			log.Fatalf("Failed to serve gRPC server, err: %v", err)
		}
	}()

	<-ctx.Done()

	srv.GracefulStop()
	log.Println("Product Catalog gRPC server stopped")
}

type productCatalog struct {
	pb.UnimplementedProductCatalogServiceServer
}

func loadProductCatalog() {
	log.Info("Loading Product Catalog...")
	var err error
	catalog, err = readProductFiles()
	if err != nil {
		log.Fatalf("Error reading product files: %v\n", err)
		os.Exit(1)
	}

	// Default reload interval is 10 seconds
	interval := DEFAULT_RELOAD_INTERVAL
	si := os.Getenv("PRODUCT_CATALOG_RELOAD_INTERVAL")
	if si != "" {
		interval, _ = strconv.Atoi(si)
		if interval <= 0 {
			interval = DEFAULT_RELOAD_INTERVAL
		}
	}
	log.Infof("Product Catalog reload interval: %d", interval)

	ticker := time.NewTicker(time.Duration(interval) * time.Second)

	go func() {
		for {
			select {
			case <-ticker.C:
				log.Info("Reloading Product Catalog...")
				catalog, err = readProductFiles()
				if err != nil {
					log.Errorf("Error reading product files: %v", err)
					continue
				}
			}
		}
	}()
}

func readProductFiles() ([]*pb.Product, error) {

	// find all .json files in the products directory
	entries, err := os.ReadDir("./products")
	if err != nil {
		return nil, err
	}

	jsonFiles := make([]fs.FileInfo, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			jsonFiles = append(jsonFiles, info)
		}
	}

	// read the contents of each .json file and unmarshal into a ListProductsResponse
	// then append the products to the catalog
	var products []*pb.Product
	for _, f := range jsonFiles {
		jsonData, err := os.ReadFile("./products/" + f.Name())
		if err != nil {
			return nil, err
		}

		var res pb.ListProductsResponse
		unmarshalOpts := protojson.UnmarshalOptions{
			DiscardUnknown: true, // Allow fields like _panic, _comment for documentation
		}
		if err := unmarshalOpts.Unmarshal(jsonData, &res); err != nil {
			return nil, err
		}

		products = append(products, res.Products...)
	}

	log.Infof("Loaded %d products", len(products))

	return products, nil
}

func mustMapEnv(target *string, key string) {
	value, present := os.LookupEnv(key)
	if !present {
		log.Fatalf("Environment Variable Not Set: %q", key)
	}
	*target = value
}

func (p *productCatalog) Check(ctx context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

func (p *productCatalog) Watch(req *healthpb.HealthCheckRequest, ws healthpb.Health_WatchServer) error {
	return status.Errorf(codes.Unimplemented, "health check via Watch not implemented")
}

// applyDiscountPricing applies dynamic discounts with A/B testing and error injection
func applyDiscountPricing(ctx context.Context, products []*pb.Product) []*pb.Product {
	span := trace.SpanFromContext(ctx)

	// Feature flag check for discount feature
	client := openfeature.NewClient("productCatalog")
	discountEnabled, _ := client.BooleanValue(
		ctx, "discountFeature", true, openfeature.EvaluationContext{},
	)

	if !discountEnabled {
		return products
	}

	// Create a copy to avoid mutating original catalog
	discountedProducts := make([]*pb.Product, len(products))

	for i, product := range products {
		// ═══════════════════════════════════════════════════════════════════════════
		// PANIC #1: Empty Categories Array
		// ═══════════════════════════════════════════════════════════════════════════
		// SUBTLE BUG: Extract product category prefix for category-specific logic
		// Looks reasonable - getting first word of first category
		// Example: "telescopes" → "telescopes", "telescopes, advanced" → "telescopes"
		// TRIGGERS: "categories": [] in JSON (EDGE_CASE_3, EDGE_CASE_8)
		// ERROR: panic: runtime error: index out of range [0] with length 0
		categoryPrefix := strings.Split(product.Categories[0], ",")[0] // PANIC: if categories is empty!

		// Use category for logging (seems harmless)
		span.AddEvent(fmt.Sprintf("Processing product in category: %s", categoryPrefix))

		// Deep copy the product
		discountedProducts[i] = &pb.Product{
			Id:          product.Id,
			Name:        product.Name,
			Description: product.Description,
			Picture:     product.Picture,
			PriceUsd: &pb.Money{
				CurrencyCode: product.PriceUsd.CurrencyCode,
				Units:        product.PriceUsd.Units,
				Nanos:        product.PriceUsd.Nanos,
			},
			Categories:  product.Categories,
			MinDiscount: product.MinDiscount,
			MaxDiscount: product.MaxDiscount,
		}

		// Calculate discount with error injection - now using product discount range from JSON
		discount := calculateDiscount(product)

		// Apply discount to price
		if discount > 0 {
			originalPrice := float64(product.PriceUsd.Units) + float64(product.PriceUsd.Nanos)/1e9
			discountedPrice := originalPrice * (1 - discount/100.0)

			discountedProducts[i].PriceUsd.Units = int64(discountedPrice)
			discountedProducts[i].PriceUsd.Nanos = int32((discountedPrice - float64(int64(discountedPrice))) * 1e9)

			span.AddEvent(fmt.Sprintf("Discount applied: %s - %.1f%%", product.Id, discount))
		}
	}

	return discountedProducts
}

// calculateDiscount determines discount percentage from product JSON
// SUBTLE BUGS: Looks reasonable but has hidden edge cases
func calculateDiscount(product *pb.Product) float64 {
	minDiscount := product.MinDiscount
	maxDiscount := product.MaxDiscount
	priceUnits := product.PriceUsd.Units

	// Calculate discount tier based on price
	// Common pattern: different discount levels for different price ranges
	discountTiers := []int64{50, 100, 200, 500, 1000}

	// Find which tier this product belongs to
	tierIndex := 0
	for i, tier := range discountTiers {
		if priceUnits > tier {
			tierIndex = i + 1
		}
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// PANIC #2: Price Tier Array Overflow
	// ═══════════════════════════════════════════════════════════════════════════
	// Apply tier bonus - looks safe but...
	// SUBTLE BUG: If product is very expensive (>$1000), tierIndex = 5
	// But we only have 5 tiers (0-4), so accessing index 5 crashes!
	// TRIGGERS: "units": > 1000 in JSON (9SIQT8TOJO $3,599, EDGE_CASE_1 $15,000)
	// ERROR: panic: runtime error: index out of range [5] with length 5
	tierBonuses := []int32{0, 2, 5, 10, 15} // Only 5 elements (indices 0-4)
	tierBonus := tierBonuses[tierIndex]     // PANIC: if tierIndex = 5!

	// Calculate base discount with tier adjustment
	adjustedMax := maxDiscount + tierBonus
	discountRange := adjustedMax - minDiscount

	// ═══════════════════════════════════════════════════════════════════════════
	// PANIC #3: Division by Zero - Discount Range
	// ═══════════════════════════════════════════════════════════════════════════
	// SUBTLE: Division by zero when min = max (looks safe because we added tierBonus)
	// But if tierBonus = 0 (price < $50) and min = max, still crashes!
	// TRIGGERS: min_discount = max_discount AND tierBonus = 0
	//           (LS4PSXUNUM min=0 max=0, EDGE_CASE_2 min=10 max=10 with price=$45)
	// ERROR: panic: runtime error: integer divide by zero
	efficiency := adjustedMax / discountRange // PANIC: when discountRange = 0!

	// Use efficiency in calculation
	baseDiscount := float64(minDiscount)
	if efficiency > 0 {
		baseDiscount = float64(minDiscount) + float64(efficiency)*0.01
	}

	// Random discount
	discount := baseDiscount + rand.Float64()*float64(discountRange)

	return discount
}

// sortProductsWithRecommendations sorts products by discount-to-price ratio
// SUBTLE BUG: Looks safe but has hidden edge cases
func sortProductsWithRecommendations(ctx context.Context, products []*pb.Product) []*pb.Product {
	if len(products) == 0 {
		return products
	}

	span := trace.SpanFromContext(ctx)
	span.AddEvent("Sorting products by value")

	// Group products by category for category-based sorting
	categoryMap := make(map[string][]*pb.Product)
	for _, product := range products {
		// ═══════════════════════════════════════════════════════════════════════════
		// PANIC #4: Empty Categories Array (Sorting)
		// ═══════════════════════════════════════════════════════════════════════════
		// Assumes every product has at least one category
		// SUBTLE BUG: Grouping by category - looks like clean code
		// TRIGGERS: "categories": [] in JSON (EDGE_CASE_3, EDGE_CASE_8)
		// ERROR: panic: runtime error: index out of range [0] with length 0
		primaryCategory := product.Categories[0] // PANIC: if categories is empty!
		categoryMap[primaryCategory] = append(categoryMap[primaryCategory], product)
	}

	// Calculate average discount per category (for category ranking)
	categoryAvgDiscount := make(map[string]float64)
	for category, prods := range categoryMap {
		totalDiscount := 0
		for _, p := range prods {
			totalDiscount += int(p.MaxDiscount)
		}
		// Looks safe - we have products in prods... but what if we filtered some out?
		categoryAvgDiscount[category] = float64(totalDiscount) / float64(len(prods))
	}

	// Sort products: high-discount categories first
	sortedProducts := make([]*pb.Product, 0, len(products))

	// Find category with highest average discount
	var bestCategory string
	var maxAvg float64
	for cat, avg := range categoryAvgDiscount {
		if avg > maxAvg {
			maxAvg = avg
			bestCategory = cat
		}
	}

	// Add products from best category first
	sortedProducts = append(sortedProducts, categoryMap[bestCategory]...)

	// Add remaining products
	for category, prods := range categoryMap {
		if category != bestCategory {
			sortedProducts = append(sortedProducts, prods...)
		}
	}

	span.SetAttributes(attribute.Int("products.sorted", len(sortedProducts)))
	return sortedProducts
}

func (p *productCatalog) ListProducts(ctx context.Context, req *pb.Empty) (*pb.ListProductsResponse, error) {
	startTime := time.Now()
	defer func() {
		duration := float64(time.Since(startTime).Microseconds()) / 1000.0 // Convert to milliseconds
		listProductsHistogram.Record(ctx, duration)
	}()

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.Int("app.products.count", len(catalog)),
	)

	// Use the pre-initialized counter
	productsCounter.Add(ctx, 1)

	// Apply discount pricing with A/B testing
	productsWithDiscount := applyDiscountPricing(ctx, catalog)

	// Sort products with recommendations
	sortedProducts := sortProductsWithRecommendations(ctx, productsWithDiscount)

	return &pb.ListProductsResponse{Products: sortedProducts}, nil
}

func (p *productCatalog) GetProduct(ctx context.Context, req *pb.GetProductRequest) (*pb.Product, error) {
	startTime := time.Now()
	defer func() {
		duration := float64(time.Since(startTime).Microseconds()) / 1000.0 // Convert to milliseconds
		getProductHistogram.Record(ctx, duration)
	}()

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("app.product.id", req.Id),
	)

	// Record metrics
	productCounter, _ = meter.Int64Counter("product_catalog.get_product.count")
	productCounter.Add(ctx, 1)

	// GetProduct will fail on a specific product when feature flag is enabled
	if p.checkProductFailure(ctx, req.Id) {
		msg := fmt.Sprintf("Error: Product Catalog Fail Feature Flag Enabled")
		span.SetStatus(otelcodes.Error, msg)
		span.AddEvent(msg)

		// Record error metrics
		errorCounter.Add(ctx, 1)
		errorCounter, _ = meter.Int64Counter("product_catalog.get_product.errors")
		errorCounter.Add(ctx, 1)

		return nil, status.Errorf(codes.Internal, msg)
	}

	var found *pb.Product
	for _, product := range catalog {
		if req.Id == product.Id {
			found = product
			break
		}
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// PANIC #5: Nil Pointer - Analytics Before Nil Check
	// ═══════════════════════════════════════════════════════════════════════════
	// SUBTLE BUG: Calculate product popularity based on name length and categories
	// Looks like harmless analytics code that runs before returning product
	// TRIGGERS: Request with non-existent product ID
	// ERROR: panic: runtime error: invalid memory address or nil pointer dereference
	nameComplexity := len(found.Name)      // PANIC: if found = nil!
	categoryCount := len(found.Categories) // PANIC: if found = nil!

	// Use these for logging/metrics (seems reasonable)
	span.SetAttributes(
		attribute.Int("product.name_complexity", nameComplexity),
		attribute.Int("product.category_count", categoryCount),
	)

	if found == nil {
		msg := fmt.Sprintf("Product Not Found: %s", req.Id)
		span.SetStatus(otelcodes.Error, msg)
		span.AddEvent(msg)

		// Record error metrics
		errorCounter.Add(ctx, 1)
		notFoundCounter, _ := meter.Int64Counter("product_catalog.get_product.not_found")
		notFoundCounter.Add(ctx, 1)

		return nil, status.Errorf(codes.NotFound, msg)
	}

	span.AddEvent("Product Found")
	span.SetAttributes(
		attribute.String("app.product.id", req.Id),
		attribute.String("app.product.name", found.Name),
	)

	// Calculate bulk pricing for analytics (seems reasonable)
	if len(found.BulkTiers) > 0 {
		// Simulate calculating bulk discount for quantity 10
		bulkDiscount := getBulkDiscount(found, 10)
		span.SetAttributes(attribute.Float64("product.bulk_discount", bulkDiscount))
	}

	// Fetch related products for recommendations
	relatedProducts := getRelatedProducts(found)
	if len(relatedProducts) > 0 {
		relatedIds := make([]string, len(relatedProducts))
		for i, rp := range relatedProducts {
			relatedIds[i] = rp.Id
		}
		span.SetAttributes(attribute.StringSlice("product.related_ids", relatedIds))
	}

	return found, nil
}

func (p *productCatalog) SearchProducts(ctx context.Context, req *pb.SearchProductsRequest) (*pb.SearchProductsResponse, error) {
	startTime := time.Now()
	defer func() {
		duration := float64(time.Since(startTime).Microseconds()) / 1000.0 // Convert to milliseconds
		searchProductsHistogram.Record(ctx, duration)
	}()

	span := trace.SpanFromContext(ctx)

	// Record metrics
	searchCounter, _ = meter.Int64Counter("product_catalog.search_products.count")
	searchCounter.Add(ctx, 1)

	// Search with tag-based relevance scoring
	type productScore struct {
		product *pb.Product
		score   int
	}

	scoredResults := make([]productScore, 0)

	for _, product := range catalog {
		matched := false
		score := 0

		// Standard name/description search
		if strings.Contains(strings.ToLower(product.Name), strings.ToLower(req.Query)) {
			matched = true
			score += 100
		}
		if strings.Contains(strings.ToLower(product.Description), strings.ToLower(req.Query)) {
			matched = true
			score += 50
		}

		// SUBTLE BUG: Tag-based relevance scoring
		if matched {
			tagScore := calculateTagRelevance(product, req.Query)
			score += tagScore

			scoredResults = append(scoredResults, productScore{
				product: product,
				score:   score,
			})
		}
	}

	// Sort by score (highest first)
	for i := 0; i < len(scoredResults)-1; i++ {
		for j := 0; j < len(scoredResults)-i-1; j++ {
			if scoredResults[j].score < scoredResults[j+1].score {
				scoredResults[j], scoredResults[j+1] = scoredResults[j+1], scoredResults[j]
			}
		}
	}

	// Extract sorted products
	result := make([]*pb.Product, len(scoredResults))
	for i, ps := range scoredResults {
		result[i] = ps.product
	}

	span.SetAttributes(
		attribute.Int("app.products_search.count", len(result)),
		attribute.String("app.products_search.query", req.Query),
	)

	// Record search results metric
	searchResultsCounter, _ = meter.Int64Counter("product_catalog.search_products.results")
	searchResultsCounter.Add(ctx, int64(len(result)))

	return &pb.SearchProductsResponse{Results: result}, nil
}

// calculateTagRelevance scores product based on tag matching
// SUBTLE BUGS: Looks like good search optimization, but has edge cases
func calculateTagRelevance(product *pb.Product, searchQuery string) int {
	score := 0

	// Extract first letter of each tag for quick matching
	// Common optimization: check tag prefixes
	for _, tag := range product.Tags {
		tagLower := strings.ToLower(tag)

		// ═══════════════════════════════════════════════════════════════════════════
		// PANIC #6: Empty Tag String
		// ═══════════════════════════════════════════════════════════════════════════
		// SUBTLE BUG: Extract first letter for prefix matching optimization
		// TRIGGERS: "tags": ["valid", "", "another"] - empty string in array
		//           (EDGE_CASE_4: Telescope Cleaning Brush)
		// ERROR: panic: runtime error: index out of range [0] with length 0
		firstLetter := tagLower[0] // PANIC: if tag is empty string!

		if strings.ContainsRune(strings.ToLower(searchQuery), rune(firstLetter)) {
			score += 10
		}

		// Full tag match bonus
		if strings.Contains(strings.ToLower(searchQuery), tagLower) {
			score += 25
		}
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// PANIC #7: Empty Tags Array
	// ═══════════════════════════════════════════════════════════════════════════
	// Normalize score by number of tags (average relevance)
	// SUBTLE BUG: Division by zero if product has no tags
	// TRIGGERS: "tags": [] in JSON (EDGE_CASE_5: Astronomy Mobile App)
	// ERROR: panic: runtime error: integer divide by zero
	avgScore := score / len(product.Tags) // PANIC: if len(tags) = 0!

	return avgScore
}

// getBulkDiscount calculates discount for bulk purchases
// SUBTLE BUGS: Business logic looks correct but has edge cases
func getBulkDiscount(product *pb.Product, quantity int32) float64 {
	if len(product.BulkTiers) == 0 {
		return 0
	}

	// Find matching tier for quantity
	var matchedTier *pb.BulkPriceTier
	for _, tier := range product.BulkTiers {
		if quantity >= tier.MinQuantity && quantity <= tier.MaxQuantity {
			matchedTier = tier
			break
		}
	}

	if matchedTier == nil {
		// Use last tier as default for large quantities
		matchedTier = product.BulkTiers[len(product.BulkTiers)-1]
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// PANIC #8: Bulk Tier Min Quantity Zero
	// ═══════════════════════════════════════════════════════════════════════════
	// Calculate discount efficiency per unit
	// SUBTLE BUG: Division by zero if MinQuantity = 0
	// Looks logical: "discount per minimum purchase unit" - standard bulk pricing
	// TRIGGERS: "minQuantity": 0 in bulkTiers (EDGE_CASE_6: Bulk Eyepiece Set)
	// ERROR: panic: runtime error: integer divide by zero
	discountPerUnit := matchedTier.DiscountPercent / matchedTier.MinQuantity // PANIC!

	// Calculate total bulk discount
	quantityRange := matchedTier.MaxQuantity - matchedTier.MinQuantity

	// ═══════════════════════════════════════════════════════════════════════════
	// PANIC #9: Bulk Tier Range Zero
	// ═══════════════════════════════════════════════════════════════════════════
	// SUBTLE BUG: Division by zero if MinQuantity = MaxQuantity
	// Looks safe: "normalize discount by quantity range" - reasonable logic
	// TRIGGERS: "minQuantity": X, "maxQuantity": X (same value)
	//           (EDGE_CASE_7: Star Atlas Book, min=5 max=5)
	// ERROR: panic: runtime error: integer divide by zero
	normalizedDiscount := matchedTier.DiscountPercent / quantityRange // PANIC!

	finalDiscount := float64(discountPerUnit) + float64(normalizedDiscount)*0.1

	return finalDiscount
}

// getRelatedProducts fetches related products for cross-sell
// SUBTLE BUGS: Looks like standard recommendation logic
func getRelatedProducts(product *pb.Product) []*pb.Product {
	if len(product.RelatedProductIds) == 0 {
		return nil
	}

	related := make([]*pb.Product, 0, len(product.RelatedProductIds))

	for _, relatedId := range product.RelatedProductIds {
		// Find related product in catalog
		var found *pb.Product
		for _, p := range catalog {
			if p.Id == relatedId {
				found = p
				break
			}
		}

		// ═══════════════════════════════════════════════════════════════════════════
		// PANIC #10: Invalid Related Product ID (Nil Pointer)
		// ═══════════════════════════════════════════════════════════════════════════
		// Calculate similarity score for analytics
		// SUBTLE BUG: Assumes related product ID always exists in catalog
		// TRIGGERS: "relatedProductIds": ["VALID", "INVALID_PRODUCT_123"]
		//           (EDGE_CASE_8: Telescope Finder Scope)
		// ERROR: panic: runtime error: invalid memory address or nil pointer dereference
		nameSimilarity := len(found.Name) - len(product.Name) // PANIC: if found = nil!

		// Check category overlap
		categorySimilarity := 0
		for _, cat1 := range found.Categories { // PANIC: if found = nil!
			for _, cat2 := range product.Categories {
				if cat1 == cat2 {
					categorySimilarity++
				}
			}
		}

		// ═══════════════════════════════════════════════════════════════════════════
		// PANIC #11: Related Products Empty Categories
		// ═══════════════════════════════════════════════════════════════════════════
		// Calculate recommendation strength based on category overlap
		// SUBTLE BUG: Division by zero if source product has no categories
		// TRIGGERS: "categories": [] AND has "relatedProductIds"
		//           (EDGE_CASE_8: Telescope Finder Scope)
		// ERROR: panic: runtime error: integer divide by zero
		recommendationStrength := categorySimilarity / len(product.Categories) // PANIC!

		log.Debugf("Related product %s: similarity=%d, strength=%d",
			relatedId, nameSimilarity, recommendationStrength)

		if found != nil {
			related = append(related, found)
		}
	}

	return related
}

func (p *productCatalog) checkProductFailure(ctx context.Context, id string) bool {
	if id != "OLJCESPC7Z" {
		return false
	}

	client := openfeature.NewClient("productCatalog")
	failureEnabled, _ := client.BooleanValue(
		ctx, "productCatalogFailure", false, openfeature.EvaluationContext{},
	)
	return failureEnabled
}

func createClient(ctx context.Context, svcAddr string) (*grpc.ClientConn, error) {
	return grpc.DialContext(ctx, svcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
}

/*
================================================================================
PRODUCT CATALOG - INTENTIONAL PANICS FOR OBSERVABILITY TESTING
================================================================================

11 Natural Panics (Real features with realistic bugs - missing validations)

PANIC #1: Empty categories in discount calculation
  Line: 404 | Bug: product.Categories[0] | Trigger: categories=[]
  Product: EDGE_CASE_3, EDGE_CASE_8 | API: ListProducts

PANIC #2: Price tier overflow
  Line: 466 | Bug: tierBonuses[tierIndex] | Trigger: price > $1000
  Product: EDGE_CASE_1 ($15k) | API: ListProducts

PANIC #3: Division by zero in discount range
  Line: 474 | Bug: adjustedMax / discountRange | Trigger: min=max discount
  Product: EDGE_CASE_2 | API: ListProducts

PANIC #4: Empty categories in sorting
  Line: 502 | Bug: product.Categories[0] | Trigger: categories=[]
  Product: EDGE_CASE_3, EDGE_CASE_8 | API: ListProducts

PANIC #5: Nil pointer in analytics
  Line: 608-609 | Bug: len(found.Name) before nil check | Trigger: invalid ID
  Product: Any invalid ID | API: GetProduct

PANIC #6: Empty tag string
  Line: 723 | Bug: tagLower[0] | Trigger: tags contains ""
  Product: EDGE_CASE_4 | API: SearchProducts

PANIC #7: Empty tags array division
  Line: 737 | Bug: score / len(tags) | Trigger: tags=[]
  Product: EDGE_CASE_5 | API: SearchProducts

PANIC #8: Bulk tier min quantity zero
  Line: 766 | Bug: discount / minQuantity | Trigger: minQuantity=0
  Product: EDGE_CASE_6 | API: GetProduct

PANIC #9: Bulk tier range zero
  Line: 773 | Bug: discount / quantityRange | Trigger: min=max quantity
  Product: EDGE_CASE_7 | API: GetProduct

PANIC #10: Invalid related product
  Line: 801,805 | Bug: found.Name when found=nil | Trigger: invalid relatedProductIds
  Product: EDGE_CASE_8 | API: GetProduct

PANIC #11: Empty categories in relations
  Line: 815 | Bug: similarity / len(categories) | Trigger: categories=[]
  Product: EDGE_CASE_8 | API: GetProduct

Quick Test:
  PORT=$(docker ps --format "{{.Ports}}" | grep 3550 | sed 's/.*:\([0-9]*\)->3550.)
  grpcurl -plaintext -d '{"id":"EDGE_CASE_1"}' localhost:$PORT oteldemo.ProductCatalogService/GetProduct

Safe Mode:
  Remove EDGE_CASE_* products from products.json

Last Updated: October 10, 2025
================================================================================
*/
