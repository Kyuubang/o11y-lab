// order-api: orchestrates calls to inventory-api over HTTP (trace context is
// propagated with otelhttp), keeps orders in memory and emits business metrics.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"o11ylab/internal/httpx"
	"o11ylab/internal/telemetry"
)

const serviceName = "order-api"

type Item struct {
	ID    int64   `json:"id"`
	SKU   string  `json:"sku"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Stock int     `json:"stock"`
}

type Order struct {
	ID        int64     `json:"id"`
	ItemID    int64     `json:"item_id"`
	SKU       string    `json:"sku"`
	Quantity  int       `json:"quantity"`
	Total     float64   `json:"total"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type orderInput struct {
	ItemID   int64 `json:"item_id"`
	Quantity int   `json:"quantity"`
}

// upstreamError carries the inventory-api status so we can map it back.
type upstreamError struct {
	Status int
	Body   string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("inventory-api returned %d: %s", e.Status, e.Body)
}

type app struct {
	log          *slog.Logger
	tracer       trace.Tracer
	client       *http.Client
	inventoryURL string

	mu     sync.RWMutex
	orders map[int64]Order
	nextID atomic.Int64

	ordersTotal metric.Int64Counter
	orderValue  metric.Float64Histogram
}

func main() {
	log := telemetry.NewLogger(serviceName)
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx := context.Background()
	shutdown, err := telemetry.Setup(ctx, serviceName, telemetry.Env("SERVICE_VERSION", "dev"))
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer shutdown(context.Background())

	meter := otel.Meter(serviceName)
	a := &app{
		log:          log,
		tracer:       otel.Tracer(serviceName),
		inventoryURL: telemetry.Env("INVENTORY_URL", "http://localhost:8081"),
		orders:       map[int64]Order{},
		client: &http.Client{
			Timeout:   5 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
	}
	if a.ordersTotal, err = meter.Int64Counter("orders.created",
		metric.WithDescription("Orders processed, by outcome")); err != nil {
		return err
	}
	if a.orderValue, err = meter.Float64Histogram("orders.value",
		metric.WithDescription("Order total value"), metric.WithUnit("USD"),
		metric.WithExplicitBucketBoundaries(10, 25, 50, 100, 250, 500, 1000, 2500, 5000)); err != nil {
		return err
	}

	rt := httpx.NewRouter(log)
	rt.Handle("GET /healthz", a.health)
	rt.Handle("GET /catalog", a.catalog)
	rt.Handle("GET /orders", a.listOrders)
	rt.Handle("GET /orders/{id}", a.getOrder)
	rt.Handle("POST /orders", a.createOrder)

	return httpx.Serve(log, ":"+telemetry.Env("PORT", "8080"), rt.Handler(serviceName))
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *app) catalog(w http.ResponseWriter, r *http.Request) {
	var items []Item
	if err := a.call(r.Context(), http.MethodGet, "/items", nil, &items); err != nil {
		a.upstreamErr(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, items)
}

func (a *app) listOrders(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	out := make([]Order, 0, len(a.orders))
	for _, o := range a.orders {
		out = append(out, o)
	}
	a.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > 100 {
		out = out[:100]
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (a *app) getOrder(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Error(w, r, http.StatusBadRequest, errors.New("invalid id"))
		return
	}
	a.mu.RLock()
	o, ok := a.orders[id]
	a.mu.RUnlock()
	if !ok {
		httpx.Error(w, r, http.StatusNotFound, errors.New("order not found"))
		return
	}
	httpx.JSON(w, http.StatusOK, o)
}

// createOrder: look up the item, validate stock, reserve it, then record the order.
func (a *app) createOrder(w http.ResponseWriter, r *http.Request) {
	var in orderInput
	if err := httpx.Decode(r, &in); err != nil || in.ItemID <= 0 || in.Quantity <= 0 {
		httpx.Error(w, r, http.StatusBadRequest, errors.New("invalid body: item_id and quantity > 0 required"))
		return
	}
	ctx := r.Context()
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int64("order.item_id", in.ItemID), attribute.Int("order.quantity", in.Quantity))

	var item Item
	if err := a.call(ctx, http.MethodGet, fmt.Sprintf("/items/%d", in.ItemID), nil, &item); err != nil {
		a.recordOutcome(ctx, "item_lookup_failed", "")
		a.upstreamErr(w, r, err)
		return
	}

	if err := a.validate(ctx, item, in.Quantity); err != nil {
		a.recordOutcome(ctx, "rejected", item.SKU)
		httpx.Error(w, r, http.StatusConflict, err)
		return
	}

	var reserved Item
	body := map[string]int{"quantity": in.Quantity}
	if err := a.callWithRetry(ctx, http.MethodPost, fmt.Sprintf("/items/%d/reserve", in.ItemID), body, &reserved); err != nil {
		a.recordOutcome(ctx, "reserve_failed", item.SKU)
		a.upstreamErr(w, r, err)
		return
	}

	o := Order{
		ID:        a.nextID.Add(1),
		ItemID:    item.ID,
		SKU:       item.SKU,
		Quantity:  in.Quantity,
		Total:     item.Price * float64(in.Quantity),
		Status:    "confirmed",
		CreatedAt: time.Now().UTC(),
	}
	a.mu.Lock()
	a.orders[o.ID] = o
	a.mu.Unlock()

	a.recordOutcome(ctx, "confirmed", item.SKU)
	a.orderValue.Record(ctx, o.Total, metric.WithAttributes(attribute.String("sku", item.SKU)))
	a.log.InfoContext(ctx, "order confirmed", "order_id", o.ID, "sku", o.SKU, "quantity", o.Quantity,
		"total", o.Total, "remaining_stock", reserved.Stock)
	httpx.JSON(w, http.StatusCreated, o)
}

// validate is a pure in-process step, shown as its own span in the trace.
func (a *app) validate(ctx context.Context, item Item, qty int) error {
	_, span := a.tracer.Start(ctx, "order.validate")
	defer span.End()
	if item.Stock < qty {
		err := fmt.Errorf("insufficient stock for %s: have %d, want %d", item.SKU, item.Stock, qty)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

func (a *app) recordOutcome(ctx context.Context, outcome, sku string) {
	a.ordersTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", outcome), attribute.String("sku", sku)))
}

// callWithRetry retries transient 5xx responses with a small backoff, so retries show up in traces.
func (a *app) callWithRetry(ctx context.Context, method, path string, body, out any) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = a.call(ctx, method, path, body, out)
		var ue *upstreamError
		if err == nil || (errors.As(err, &ue) && ue.Status < 500) {
			return err
		}
		a.log.WarnContext(ctx, "upstream call failed, retrying", "path", path, "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt*50) * time.Millisecond):
		}
	}
	return err
}

func (a *app) call(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.inventoryURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return &upstreamError{Status: resp.StatusCode, Body: string(bytes.TrimSpace(data))}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (a *app) upstreamErr(w http.ResponseWriter, r *http.Request, err error) {
	var ue *upstreamError
	if errors.As(err, &ue) && ue.Status < 500 {
		httpx.Error(w, r, ue.Status, err)
		return
	}
	a.log.ErrorContext(r.Context(), "upstream failure", "error", err)
	httpx.Error(w, r, http.StatusBadGateway, err)
}
