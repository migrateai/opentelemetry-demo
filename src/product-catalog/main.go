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
		if err := protojson.Unmarshal(jsonData, &res); err != nil {
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

	return &pb.ListProductsResponse{Products: catalog}, nil
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

	var result []*pb.Product
	for _, product := range catalog {
		if strings.Contains(strings.ToLower(product.Name), strings.ToLower(req.Query)) ||
			strings.Contains(strings.ToLower(product.Description), strings.ToLower(req.Query)) {
			result = append(result, product)
		}
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
